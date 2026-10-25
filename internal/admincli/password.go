// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package admincli

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusternet"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
	"github.com/joinsider/uptimecluster/internal/config"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// resetTimeout bounds the whole reset: status call and Apply.
const resetTimeout = 30 * time.Second

// runResetPassword sets a new password for a local user (ADR 0019). It runs
// next to the running node and proposes the same PutEntity/User command as a
// password change, through the leader, authenticated with this node's voter
// certificate. The FSM ends all sessions of the user with it. The plaintext
// password is read from stdin or a file, never from the arguments, and never
// leaves this process.
func runResetPassword(ctx context.Context, cfg config.Config, args []string, env Env) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	username := fs.String("username", "", "user whose password is reset (required)")
	passwordFile := fs.String("password-file", "", "read the new password from this file instead of stdin")
	enable := fs.Bool("enable", false, "also enable the user if it is disabled")
	fs.Usage = func() {
		_, _ = fmt.Fprint(env.Stderr, "usage: uptimecluster admin reset-password --username <name> [--password-file <file>] [--enable]\n\n"+
			"Sets a new password and ends all sessions of the user. The node must be running and the\n"+
			"cluster must have a leader. The password is read from stdin (twice, without echo, on a terminal).\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError("%v", err)
	}
	if fs.NArg() > 0 {
		return usageError("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*username) == "" {
		return usageError("--username is required")
	}
	if cfg.AuthMode != config.AuthLocal {
		return errors.New("password reset is only available with local authentication (AUTH_MODE=local)")
	}

	ctx, cancel := context.WithTimeout(ctx, resetTimeout)
	defer cancel()

	id, err := nodeID(cfg.DataDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, sqlite.StateFile)); err != nil {
		return fmt.Errorf("no state in %s: %w", cfg.DataDir, err)
	}
	state, err := sqlite.OpenState(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	u, err := state.GetUserByUsername(ctx, strings.TrimSpace(*username))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no local user %q", *username)
	}
	if err != nil {
		return fmt.Errorf("read user: %w", err)
	}

	password, err := readNewPassword(env, *passwordFile)
	if err != nil {
		return err
	}
	if err := auth.CheckPassword(password); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	cmd, err := cluster.NewPutUser(uuid.NewString(), u.TenantID, u.ID, cluster.UserDocument{
		Username: u.Username, DisplayName: u.DisplayName, PasswordHash: hash, Role: u.Role,
		Disabled: u.Disabled && !*enable,
	}, u.Revision)
	if err != nil {
		return err
	}
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM, Id: "cli:" + id}

	sec, err := voterSecurity(ctx, cfg.DataDir, id, state)
	if err != nil {
		return err
	}
	if err := applyOnLeader(ctx, cfg.ClusterAdvertise, sec, cmd); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(env.Stdout, "Password of %q reset; all sessions of the user ended.\n", u.Username)
	if u.Disabled {
		if *enable {
			_, _ = fmt.Fprintln(env.Stdout, "The user is enabled again.")
		} else {
			_, _ = fmt.Fprintln(env.Stdout, "The user is still disabled; use --enable to enable it.")
		}
	}
	return nil
}

// readNewPassword reads the new password from file, or from stdin: twice
// without echo on a terminal, a single line otherwise (scripts, pipes).
func readNewPassword(env Env, file string) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(file) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return "", fmt.Errorf("read password file: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	if f, ok := env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprint(env.Stderr, "New password: ")
		first, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		_, _ = fmt.Fprint(env.Stderr, "Repeat password: ")
		second, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if string(first) != string(second) {
			return "", errors.New("the passwords do not match")
		}
		return string(first), nil
	}
	line, err := bufio.NewReader(io.LimitReader(env.Stdin, auth.MaxPasswordLen*4+2)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// voterSecurity loads this node's certificate and trusts the cluster CAs and
// revocations of the local state, like the running node does.
func voterSecurity(ctx context.Context, dataDir, nodeID string, state store.State) (clusternet.Security, error) {
	cert := &clusterpki.NodeCert{DataDir: dataDir, NodeID: nodeID}
	if err := cert.Load(); err != nil {
		return clusternet.Security{}, err
	}
	leaf := cert.Leaf()
	if leaf == nil {
		return clusternet.Security{}, errors.New("this node has no cluster certificate yet")
	}
	self, err := pki.IdentityOf(leaf)
	if err != nil {
		return clusternet.Security{}, err
	}
	if self.Role != pki.RoleVoter {
		return clusternet.Security{}, errors.New("this node is not a voter")
	}
	roots, _, err := (&clusterpki.Authority{State: state}).Bundle(ctx)
	if err != nil {
		return clusternet.Security{}, fmt.Errorf("cluster CA: %w", err)
	}
	revocations := &clusterpki.Revocations{}
	if err := revocations.Reload(ctx, state); err != nil {
		return clusternet.Security{}, err
	}
	return clusternet.Security{
		Holder: &cert.Holder,
		Verifier: &pki.Verifier{
			ClusterID:   self.ClusterID,
			Roots:       func() []*x509.Certificate { return roots },
			Revoked:     revocations.Revoked,
			RevokedNode: revocations.RevokedNode,
		},
	}, nil
}

// applyOnLeader asks the local node for the leader and proposes cmd there.
// Followers never forward commands they receive (AP-2.3), so the CLI does.
func applyOnLeader(ctx context.Context, localAddr string, sec clusternet.Security, cmd *pb.Command) error {
	local, err := clusternet.DialVoter(localAddr, sec)
	if err != nil {
		return fmt.Errorf("connect to this node at %s: %w", localAddr, err)
	}
	defer func() { _ = local.Close() }()
	st, err := pb.NewClusterServiceClient(local).GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil {
		return fmt.Errorf("this node at %s does not answer (is it running?): %w", localAddr, err)
	}
	leaderAddr := st.GetLeaderAddress()
	if leaderAddr == "" {
		return errors.New("the cluster has no leader: without quorum, see \"uptimecluster admin recover\" in docs/betrieb/recovery.md")
	}
	conn := local
	if leaderAddr != localAddr {
		conn, err = clusternet.DialVoter(leaderAddr, sec)
		if err != nil {
			return fmt.Errorf("connect to the leader at %s: %w", leaderAddr, err)
		}
		defer func() { _ = conn.Close() }()
	}
	resp, err := pb.NewClusterServiceClient(conn).Apply(ctx, &pb.ApplyRequest{Command: cmd})
	if err != nil {
		return fmt.Errorf("apply on the leader at %s: %w", leaderAddr, err)
	}
	switch resp.GetErrorCode() {
	case "":
		return nil
	case cluster.CodeConflict:
		return fmt.Errorf("the user was changed in the meantime, or this node has not caught up with the leader yet; retry: %s", resp.GetErrorMessage())
	default:
		return fmt.Errorf("rejected: %s: %s", resp.GetErrorCode(), resp.GetErrorMessage())
	}
}
