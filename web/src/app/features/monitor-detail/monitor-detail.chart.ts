// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { DestroyRef, Signal, signal } from '@angular/core';
import type { EChartsCoreOption } from 'echarts/core';
import type { LineSeriesOption, ScatterSeriesOption } from 'echarts/charts';
import { NodeSeries, TimePoint } from './monitor-detail.model';

/** Colours of the chart; one set per colour scheme (WCAG-safe on the surfaces). */
export interface ChartPalette {
  text: string;
  muted: string;
  grid: string;
  danger: string;
  series: readonly string[];
}

export const LIGHT_PALETTE: ChartPalette = {
  text: '#1c1f24',
  muted: '#5b6270',
  grid: '#d7dbe2',
  danger: '#b42318',
  series: ['#1557b0', '#17703a', '#7b3fb0', '#8a5200', '#0e7490', '#b03a7e', '#475569', '#4d7c0f'],
};

export const DARK_PALETTE: ChartPalette = {
  text: '#e6e8ec',
  muted: '#9aa1ad',
  grid: '#353a44',
  danger: '#ff8a80',
  series: ['#8ab8ff', '#6fdc8c', '#c9a2ff', '#ffc26b', '#67e8f9', '#f9a8d4', '#a7b3c6', '#bef264'],
};

/** Translated texts of the chart. */
export interface ChartLabels {
  yAxis: string;
  failure: string;
  average: string;
}

/** Input of {@link buildLatencyOption}: live node series or bucket averages. */
export type LatencyChartInput =
  { mode: 'live'; nodes: NodeSeries[] } | { mode: 'buckets'; points: TimePoint[] };

/**
 * ECharts option of the latency chart: one line per node and red markers for
 * failed checks (live mode), or one averaged line (bucket mode). Pure function,
 * so it can be tested without a DOM.
 */
export function buildLatencyOption(
  input: LatencyChartInput,
  palette: ChartPalette,
  labels: ChartLabels,
): EChartsCoreOption {
  const series: Array<LineSeriesOption | ScatterSeriesOption> = [];
  if (input.mode === 'live') {
    input.nodes.forEach((node, i) => {
      series.push({
        type: 'line',
        name: node.nodeId,
        data: node.points,
        showSymbol: false,
        connectNulls: false,
        color: palette.series[i % palette.series.length],
      });
    });
    const failures = input.nodes.flatMap((node) => node.failures);
    if (failures.length > 0) {
      series.push({
        type: 'scatter',
        name: labels.failure,
        data: failures,
        symbolSize: 9,
        itemStyle: { color: palette.danger },
        z: 10,
      });
    }
  } else {
    series.push({
      type: 'line',
      name: labels.average,
      data: input.points,
      showSymbol: input.points.length <= 2,
      color: palette.series[0],
    });
  }

  return {
    animation: false,
    color: [...palette.series],
    grid: { left: 52, right: 16, top: 40, bottom: 44, containLabel: false },
    legend: {
      top: 0,
      textStyle: { color: palette.text },
      show: series.length > 1,
    },
    tooltip: { trigger: 'axis' },
    xAxis: {
      type: 'time',
      axisLabel: { color: palette.muted },
      axisLine: { lineStyle: { color: palette.grid } },
      splitLine: { show: false },
    },
    yAxis: {
      type: 'value',
      name: labels.yAxis,
      nameTextStyle: { color: palette.muted },
      axisLabel: { color: palette.muted },
      splitLine: { lineStyle: { color: palette.grid } },
      min: 0,
    },
    dataZoom: [{ type: 'inside' }],
    series,
  };
}

/** Whether the document uses the dark scheme: data-theme wins, else the OS setting. */
export function resolveDark(theme: string | undefined, systemDark: boolean): boolean {
  if (theme === 'dark') {
    return true;
  }
  if (theme === 'light') {
    return false;
  }
  return systemDark;
}

/**
 * Tracks the active colour scheme (the shell's data-theme attribute and the
 * OS setting) so the chart can switch palettes. Must run in an injection context.
 */
export function darkModeSignal(destroyRef: DestroyRef): Signal<boolean> {
  const hasMatchMedia = typeof window !== 'undefined' && typeof window.matchMedia === 'function';
  const query = hasMatchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  const current = (): boolean =>
    resolveDark(document.documentElement.dataset['theme'], query?.matches ?? false);
  const dark = signal(current());
  const update = (): void => dark.set(current());
  const observer = new MutationObserver(update);
  observer.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['data-theme'],
  });
  query?.addEventListener('change', update);
  destroyRef.onDestroy(() => {
    observer.disconnect();
    query?.removeEventListener('change', update);
  });
  return dark.asReadonly();
}
