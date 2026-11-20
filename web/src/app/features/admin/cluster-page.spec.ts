// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of, throwError } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ClusterService } from '../../api/gen/endpoints/cluster/cluster.service';
import { ClusterNode, ClusterNodeList } from '../../api/gen/model';
import { JoinTokenDialog } from './join-token-dialog';
import { ClusterPage } from './cluster-page';
import { RemoveNodeDialog } from './remove-node-dialog';
import { SystemAlertsService } from './system-alerts.service';

const NODES: ClusterNode[] = [
  {
    id: 'n-leader-0000000000',
    address: 'v1:7000',
    voter: true,
    leader: true,
    self: true,
    name: 'frankfurt-1',
    version: '1.4.0',
    healthy: true,
    maxCommandVersion: 2,
    capabilities: ['icmp', 'http'],
  },
  {
    id: 'n-voter-1111111111',
    address: 'v2:7000',
    voter: true,
    leader: false,
    self: false,
    name: 'berlin-1',
    healthy: false,
    replicationLagMs: 90_000,
    replicationError: 'connection reset',
  },
  {
    id: 'n-voter-2222222222',
    address: 'v3:7000',
    voter: true,
    leader: false,
    self: false,
    name: 'cologne-1',
    healthy: true,
    replicationLagMs: 500,
  },
  {
    id: 'n-learner-3333333333',
    address: 'v4:7000',
    voter: false,
    leader: false,
    self: false,
  },
];

function listOf(nodes: ClusterNode[]): ClusterNodeList {
  return { nodes, leaderId: 'n-leader-0000000000', warnings: [] };
}

/** Error as Angular's HttpClient delivers it, with an RFC 9457 body. */
function status(code: number): HttpErrorResponse {
  return new HttpErrorResponse({ status: code, error: undefined });
}

function conflict(detail: string): HttpErrorResponse {
  return new HttpErrorResponse({
    status: 409,
    error: { type: 'about:blank', title: 'Conflict', status: 409, detail },
  });
}

describe('ClusterPage', () => {
  let fixture: ComponentFixture<ClusterPage> | undefined;
  let api: {
    listClusterNodes: ReturnType<typeof vi.fn>;
    removeClusterNode: ReturnType<typeof vi.fn>;
  };
  let dialogAnswers: unknown[];
  /** Components opened by the dialog mock, in order; with the removal call in between. */
  let opened: unknown[];
  let dialog: { open: ReturnType<typeof vi.fn> };
  let snack: { open: ReturnType<typeof vi.fn> };
  let calls: string[];

  beforeEach(() => {
    calls = [];
    api = {
      listClusterNodes: vi.fn(() => of(listOf(NODES))),
      removeClusterNode: vi.fn(() => {
        calls.push('remove');
        return of({ votersLeft: 2 });
      }),
    };
    dialogAnswers = [];
    opened = [];
    dialog = {
      open: vi.fn((component: unknown) => {
        opened.push(component);
        if (component === JoinTokenDialog) {
          calls.push('join-token');
        }
        const answer = dialogAnswers.shift() ?? false;
        return { afterClosed: () => of(answer) };
      }),
    };
    snack = { open: vi.fn() };

    TestBed.configureTestingModule({
      imports: [
        ClusterPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        { provide: ClusterService, useValue: api },
        { provide: SystemAlertsService, useValue: { active: () => of([]) } },
        { provide: MatDialog, useValue: dialog },
        { provide: MatSnackBar, useValue: snack },
      ],
    });
  });

  // The page loads in its constructor, so the fixture is created only after a test set its mocks.
  afterEach(() => {
    fixture?.destroy();
  });

  function root(): HTMLElement {
    return page().nativeElement as HTMLElement;
  }

  function cards(): HTMLElement[] {
    return [...root().querySelectorAll<HTMLElement>('[data-testid="cluster-node"]')];
  }

  function cardFor(name: string): HTMLElement {
    const card = cards().find(
      (el) => el.querySelector('.node__name')?.textContent?.trim() === name,
    );
    if (!card) {
      throw new Error(`no card for ${name}`);
    }
    return card;
  }

  function actionButton(name: string, action: 'remove' | 'replace'): HTMLButtonElement {
    const label = `admin.cluster.actions.${action}: ${name}`;
    const button = cardFor(name).querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
    if (!button) {
      throw new Error(`no ${action} button for ${name}`);
    }
    return button;
  }

  /** Creates the page (which loads at once) and waits for the first render. */
  async function render(): Promise<void> {
    fixture = TestBed.createComponent(ClusterPage);
    fixture.detectChanges();
    await page().whenStable();
    page().detectChanges();
  }

  /** The page after `render()`; fails the test if it was not rendered yet. */
  function page(): ComponentFixture<ClusterPage> {
    if (!fixture) {
      throw new Error('render() was not called');
    }
    return fixture;
  }

  it('renders the nodes with the leader first and the answering node marked', async () => {
    await render();

    const names = cards().map((el) => el.querySelector('.node__name')?.textContent?.trim());
    // The non-voter has no name yet, so it shows the first eight characters of its id.
    expect(names).toEqual(['frankfurt-1', 'berlin-1', 'cologne-1', 'n-learne']);
    expect(cardFor('frankfurt-1').querySelector('.badge--leader')).not.toBeNull();
    expect(cardFor('frankfurt-1').textContent).toContain('admin.cluster.node.selfBadge');
    expect(cardFor('berlin-1').querySelector('.badge--leader')).toBeNull();
  });

  it('shows the quorum summary for three voters', async () => {
    await render();

    const values = [...root().querySelectorAll('.summary dd')].map((el) => el.textContent?.trim());
    expect(values).toEqual(['3', '2', '1', 'frankfurt-1', '–']);
    expect(root().querySelector('.summary')).not.toBeNull();
  });

  it('disables removal and replacement of the leader', async () => {
    await render();

    const remove = actionButton('frankfurt-1', 'remove');
    expect(remove.disabled).toBe(true);
    expect(actionButton('frankfurt-1', 'replace').disabled).toBe(true);
    expect(actionButton('berlin-1', 'remove').disabled).toBe(false);
  });

  it('shows replication lag with severity and the replication error', async () => {
    await render();

    const lag = cardFor('berlin-1').querySelector('.lag');
    expect(lag?.classList).toContain('lag--warn');
    expect(lag?.textContent).toContain('1 min');
    // The test loader returns keys, so the interpolated error text is checked by the key only.
    expect(cardFor('berlin-1').textContent).toContain('admin.cluster.node.replicationError');
    expect(cardFor('frankfurt-1').querySelector('.lag')?.textContent?.trim()).toBe('–');
  });

  it('shows the connectivity badge with its reason only while impaired (ADR 0022)', async () => {
    const impaired: ClusterNode = {
      ...NODES[2],
      connectivityImpaired: true,
      connectivityReason: 'reference',
    };
    api.listClusterNodes.mockReturnValue(of(listOf([NODES[0], NODES[1], impaired, NODES[3]])));
    await render();

    const badge = cardFor('cologne-1').querySelector('.badge--connectivity');
    expect(badge?.textContent).toContain('admin.cluster.node.connectivity.impaired');
    expect(badge?.textContent).toContain('admin.cluster.node.connectivity.reasons.reference');
    expect(cardFor('berlin-1').querySelector('.badge--connectivity')).toBeNull();
  });

  it('shows server warnings as they are and the upgrade banner', async () => {
    api.listClusterNodes.mockReturnValue(
      of({
        ...listOf(NODES),
        warnings: ['cluster has 4 voters: an even number adds no fault tolerance'],
        upgradePending: true,
      }),
    );
    await render();

    expect(root().textContent).toContain(
      'cluster has 4 voters: an even number adds no fault tolerance',
    );
    expect(root().textContent).toContain('admin.cluster.upgradePending');
  });

  it('warns when no leader is known', async () => {
    api.listClusterNodes.mockReturnValue(of({ ...listOf(NODES), leaderId: '' }));
    await render();

    expect(root().textContent).toContain('admin.cluster.noLeader');
  });

  it('removes a node after confirmation and reports the voters left', async () => {
    dialogAnswers.push(true);
    await render();

    actionButton('berlin-1', 'remove').click();
    await page().whenStable();

    expect(opened[0]).toBe(RemoveNodeDialog);
    expect(api.removeClusterNode).toHaveBeenCalledWith('n-voter-1111111111');
    expect(snack.open).toHaveBeenCalledWith(
      expect.stringContaining('admin.cluster.saved.removed'),
      undefined,
      { duration: 5000 },
    );
  });

  it('does not remove a node when the confirmation is dismissed', async () => {
    dialogAnswers.push(false);
    await render();

    actionButton('berlin-1', 'remove').click();
    await page().whenStable();

    expect(api.removeClusterNode).not.toHaveBeenCalled();
  });

  it('opens the join token dialog only after a successful replacement', async () => {
    dialogAnswers.push(true, false);
    await render();

    actionButton('berlin-1', 'replace').click();
    await page().whenStable();
    await page().whenStable();

    expect(opened).toEqual([RemoveNodeDialog, JoinTokenDialog]);
    expect(calls).toEqual(['remove', 'join-token']);
  });

  it('does not open the join token dialog when the removal fails', async () => {
    dialogAnswers.push(true);
    api.removeClusterNode.mockReturnValue(throwError(() => conflict('last voter')));
    await render();

    actionButton('berlin-1', 'replace').click();
    await page().whenStable();
    page().detectChanges();

    expect(opened).toEqual([RemoveNodeDialog]);
    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'admin.cluster.error.conflict',
    );
    expect(root().querySelector('[role="alert"]')?.textContent).toContain('last voter');
  });

  it('shows a retry button when the cluster has no leader yet (503)', async () => {
    api.listClusterNodes.mockReturnValue(throwError(() => status(503)));
    await render();

    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'admin.error.unavailable',
    );
    expect(root().querySelector('[role="alert"]')?.querySelector('button')).not.toBeNull();
  });

  it('shows the translated error for a forbidden list call', async () => {
    api.listClusterNodes.mockReturnValue(throwError(() => status(403)));
    await render();

    expect(root().querySelector('[role="alert"]')?.textContent).toContain('admin.error.forbidden');
  });

  it('shows the SSRF setting of each node (ADR 0030)', async () => {
    const withSsrf: ClusterNode[] = [
      { ...NODES[0], ssrfProtection: true },
      { ...NODES[1], ssrfProtection: false },
      NODES[2],
      NODES[3],
    ];
    api.listClusterNodes.mockReturnValue(of(listOf(withSsrf)));
    await render();

    const facts = (name: string) =>
      [...cardFor(name).querySelectorAll('.facts > div')].find(
        (el) => el.querySelector('dt')?.textContent?.trim() === 'admin.cluster.node.ssrfProtection',
      )?.textContent ?? '';
    expect(facts('frankfurt-1')).toContain('admin.cluster.node.ssrfOn');
    expect(facts('berlin-1')).toContain('admin.cluster.node.ssrfOff');
    expect(facts('cologne-1')).toContain('–');
  });

  it('warns when the answered nodes report different SSRF settings (ADR 0030)', async () => {
    const differing: ClusterNode[] = [
      { ...NODES[0], ssrfProtection: true },
      { ...NODES[1], ssrfProtection: false },
      { ...NODES[2], ssrfProtection: true },
      NODES[3],
    ];
    api.listClusterNodes.mockReturnValue(of(listOf(differing)));
    await render();

    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'admin.cluster.ssrfMismatch',
    );
  });

  it('shows no SSRF warning when all answered nodes report the same setting', async () => {
    const equal: ClusterNode[] = [
      { ...NODES[0], ssrfProtection: true },
      { ...NODES[1], ssrfProtection: true },
      { ...NODES[2], ssrfProtection: true },
      NODES[3],
    ];
    api.listClusterNodes.mockReturnValue(of(listOf(equal)));
    await render();

    expect(root().textContent).not.toContain('admin.cluster.ssrfMismatch');
  });

  it('ignores nodes without an SSRF answer when comparing', async () => {
    const partial: ClusterNode[] = [
      { ...NODES[0], ssrfProtection: true },
      NODES[1],
      { ...NODES[2], ssrfProtection: true },
      NODES[3],
    ];
    api.listClusterNodes.mockReturnValue(of(listOf(partial)));
    await render();

    expect(root().textContent).not.toContain('admin.cluster.ssrfMismatch');
  });
});
