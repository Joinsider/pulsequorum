// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import {
  buildLatencyOption,
  DARK_PALETTE,
  LIGHT_PALETTE,
  resolveDark,
} from './monitor-detail.chart';

const LABELS = { yAxis: 'ms', failure: 'Failed', average: 'Average' };

type AnySeries = { type: string; name?: string; data?: unknown[] };

function seriesOf(option: unknown): AnySeries[] {
  return (option as { series: AnySeries[] }).series;
}

describe('buildLatencyOption', () => {
  it('draws one line per node and a failure marker series in live mode', () => {
    const option = buildLatencyOption(
      {
        mode: 'live',
        nodes: [
          {
            nodeId: 'a',
            points: [
              [1, 10],
              [2, 20],
            ],
            failures: [[2, 20]],
          },
          { nodeId: 'b', points: [[1, 30]], failures: [] },
        ],
      },
      LIGHT_PALETTE,
      LABELS,
    );
    const series = seriesOf(option);

    expect(series.map((s) => [s.type, s.name])).toEqual([
      ['line', 'a'],
      ['line', 'b'],
      ['scatter', 'Failed'],
    ]);
    expect(series[2]?.data).toEqual([[2, 20]]);
  });

  it('omits the failure series when no check failed', () => {
    const option = buildLatencyOption(
      { mode: 'live', nodes: [{ nodeId: 'a', points: [[1, 10]], failures: [] }] },
      DARK_PALETTE,
      LABELS,
    );

    expect(seriesOf(option).map((s) => s.type)).toEqual(['line']);
  });

  it('draws a single average line in bucket mode', () => {
    const option = buildLatencyOption(
      {
        mode: 'buckets',
        points: [
          [1, 5],
          [2, 7],
        ],
      },
      LIGHT_PALETTE,
      LABELS,
    );
    const series = seriesOf(option);

    expect(series).toHaveLength(1);
    expect(series[0]).toMatchObject({
      type: 'line',
      name: 'Average',
      data: [
        [1, 5],
        [2, 7],
      ],
    });
  });

  it('uses the y-axis label and the time axis', () => {
    const option = buildLatencyOption({ mode: 'buckets', points: [] }, LIGHT_PALETTE, LABELS) as {
      xAxis: { type: string };
      yAxis: { name: string };
    };

    expect(option.xAxis.type).toBe('time');
    expect(option.yAxis.name).toBe('ms');
  });
});

describe('resolveDark', () => {
  it.each<[string | undefined, boolean, boolean]>([
    ['dark', false, true],
    ['light', true, false],
    [undefined, true, true],
    [undefined, false, false],
    ['system', true, true],
  ])('theme %s with system dark %s -> %s', (theme, systemDark, expected) => {
    expect(resolveDark(theme, systemDark)).toBe(expected);
  });
});
