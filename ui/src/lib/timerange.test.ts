import { describe, it, expect } from 'vitest'

import {
  applyTimeQuery,
  localInputToRFC3339,
  presetQuery,
  TIME_PRESETS,
} from '@/lib/timerange'

describe('presetQuery', () => {
  it('returns the relative since for a known preset', () => {
    expect(presetQuery('24h')).toEqual({ since: '24h' })
  })
  it('returns an empty query for "all"', () => {
    expect(presetQuery('all')).toEqual({})
  })
  it('returns an empty query for an unknown preset', () => {
    expect(presetQuery('nope')).toEqual({})
  })
  it('every preset has a value and label', () => {
    for (const p of TIME_PRESETS) {
      expect(p.value).toBeTruthy()
      expect(p.label).toBeTruthy()
    }
  })
})

describe('applyTimeQuery', () => {
  it('sets since and until when present', () => {
    const p = new URLSearchParams()
    applyTimeQuery(p, { since: '1h', until: '2026-06-23T00:00:00Z' })
    expect(p.get('since')).toBe('1h')
    expect(p.get('until')).toBe('2026-06-23T00:00:00Z')
  })
  it('omits absent bounds', () => {
    const p = new URLSearchParams()
    applyTimeQuery(p, {})
    expect(p.toString()).toBe('')
  })
})

describe('localInputToRFC3339', () => {
  it('returns undefined for empty input', () => {
    expect(localInputToRFC3339('')).toBeUndefined()
  })
  it('returns undefined for an unparseable value', () => {
    expect(localInputToRFC3339('not-a-date')).toBeUndefined()
  })
  it('converts a datetime-local value to an RFC3339 string', () => {
    const out = localInputToRFC3339('2026-06-23T14:30')
    expect(out).toBeDefined()
    // Round-trips back to the same instant.
    expect(new Date(out!).getTime()).toBe(new Date('2026-06-23T14:30').getTime())
  })
})
