package main

// `spike vacuumbench <dir> <hb-rows>`: how expensive is VACUUM INTO as a
// snapshot mechanism if heartbeats (class B) live in the same database file
// as the class-A tables, compared to a separate state.db?

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func vacuumBench(dir string, hbRows int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "combined.db")
	_ = os.Remove(path)
	db, err := openSQLite(path, 1)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(stateSchema + hbSchema); err != nil {
		return err
	}
	fill := func(q string, n int, args func(i int) []any) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(q)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if _, err := stmt.Exec(args(i)...); err != nil {
				return err
			}
		}
		_ = stmt.Close()
		return tx.Commit()
	}
	if err := fill(`INSERT INTO config VALUES(?,?,?,?)`, 10000, func(i int) []any {
		return []any{fmt.Sprintf("mon-%06d", i), fmt.Sprintf(`{"name":"monitor %d","pad":"%0400d"}`, i, i), "v", 0}
	}); err != nil {
		return err
	}
	measure := func(label string) error {
		out := filepath.Join(dir, "snap.db")
		_ = os.Remove(out)
		start := time.Now()
		if _, err := db.Exec(`VACUUM INTO ?`, out); err != nil {
			return err
		}
		fi, _ := os.Stat(out)
		fmt.Printf("%-40s vacuum_into=%6d ms  snapshot=%7.1f MB\n", label, time.Since(start).Milliseconds(), float64(fi.Size())/1e6)
		return os.Remove(out)
	}
	if err := measure("10k config rows, 0 heartbeats"); err != nil {
		return err
	}
	const nodes = 3
	if err := fill(`INSERT INTO hb VALUES(?,?,?,?)`, hbRows, func(i int) []any {
		return []any{strconv.Itoa(i % nodes), "inc", i / nodes, int64(i) * 1000}
	}); err != nil {
		return err
	}
	return measure(fmt.Sprintf("10k config rows, %d heartbeats", hbRows))
}
