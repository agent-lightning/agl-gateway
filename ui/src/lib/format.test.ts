import { describe, it, expect, afterEach, vi } from 'vitest'

import {
  base64ByteLength,
  decodeBase64,
  formatBytes,
  formatCompact,
  formatCost,
  formatDuration,
  formatNumber,
  formatRelative,
  isJSON,
  prettyJSON,
  statusClass,
} from '@/lib/format'

describe('decodeBase64', () => {
  it('decodes ASCII', () => {
    expect(decodeBase64(btoa('hello'))).toBe('hello')
  })
  it('decodes multi-byte UTF-8', () => {
    // base64 of the UTF-8 bytes for "小" (E5 B0 8F)
    expect(decodeBase64('5bCP')).toBe('小')
  })
  it('returns empty string on invalid input', () => {
    expect(decodeBase64('!!!not base64!!!')).toBe('')
  })
})

describe('base64ByteLength', () => {
  it('is 0 for empty', () => {
    expect(base64ByteLength('')).toBe(0)
  })
  it('accounts for single and double padding', () => {
    expect(base64ByteLength(btoa('hi'))).toBe(2) // aGk= → 1 pad
    expect(base64ByteLength(btoa('h'))).toBe(1) // aA== → 2 pad
    expect(base64ByteLength(btoa('abc'))).toBe(3) // YWJj → 0 pad
  })
})

describe('prettyJSON / isJSON', () => {
  it('pretty-prints valid JSON', () => {
    expect(prettyJSON('{"a":1}')).toBe('{\n  "a": 1\n}')
    expect(isJSON('{"a":1}')).toBe(true)
  })
  it('passes non-JSON through unchanged', () => {
    expect(prettyJSON('data: x\n\n')).toBe('data: x\n\n')
    expect(isJSON('not json')).toBe(false)
  })
})

describe('formatCost', () => {
  it('renders $0 for zero', () => {
    expect(formatCost(0)).toBe('$0')
  })
  it('uses 6 decimals for sub-cent values', () => {
    expect(formatCost(0.0000012)).toBe('$0.000001')
  })
  it('uses 4 decimals for larger values', () => {
    expect(formatCost(0.5)).toBe('$0.5000')
  })
})

describe('formatNumber', () => {
  it('groups thousands', () => {
    expect(formatNumber(1234567)).toBe((1234567).toLocaleString())
  })
})

describe('formatCompact', () => {
  it('passes through small numbers', () => {
    expect(formatCompact(0)).toBe('0')
    expect(formatCompact(999)).toBe('999')
  })
  it('abbreviates thousands', () => {
    expect(formatCompact(1234)).toBe('1.2k')
    expect(formatCompact(12000)).toBe('12k')
  })
  it('abbreviates millions', () => {
    expect(formatCompact(1_500_000)).toBe('1.5M')
    expect(formatCompact(12_000_000)).toBe('12M')
  })
})

describe('formatDuration', () => {
  it('returns dash for 0', () => {
    expect(formatDuration(0)).toBe('—')
  })
  it('formats sub-second as ms', () => {
    expect(formatDuration(250)).toBe('250ms')
  })
  it('formats seconds with 2 decimals', () => {
    expect(formatDuration(1500)).toBe('1.50s')
  })
  it('formats minutes and seconds', () => {
    expect(formatDuration(65_000)).toBe('1m 5s')
  })
})

describe('formatBytes', () => {
  it('formats B / KB / MB', () => {
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(2048)).toBe('2.0 KB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.00 MB')
  })
})

describe('formatRelative', () => {
  afterEach(() => vi.useRealTimers())
  it('formats seconds/minutes/hours/days ago', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-06-23T12:00:00Z'))
    const ago = (ms: number) => new Date(Date.now() - ms).toISOString()
    expect(formatRelative(ago(5_000))).toBe('5s ago')
    expect(formatRelative(ago(5 * 60_000))).toBe('5m ago')
    expect(formatRelative(ago(3 * 3_600_000))).toBe('3h ago')
    expect(formatRelative(ago(2 * 86_400_000))).toBe('2d ago')
  })
  it('handles clock skew (future) as just now', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-06-23T12:00:00Z'))
    expect(formatRelative(new Date(Date.now() + 10_000).toISOString())).toBe(
      'just now',
    )
  })
})

describe('statusClass', () => {
  it('maps status ranges to color classes', () => {
    expect(statusClass(200)).toBe('text-success')
    expect(statusClass(404)).toBe('text-warning')
    expect(statusClass(500)).toBe('text-destructive')
    expect(statusClass(0)).toBe('text-destructive')
  })
})
