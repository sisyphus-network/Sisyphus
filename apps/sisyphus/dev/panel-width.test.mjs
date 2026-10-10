import assert from 'node:assert/strict'
import { test } from 'node:test'
import { visiblePanelWidth, stepPanelWidth } from '../src/renderer/src/lib/panel-width.ts'

test('a drag starts at the visible width after viewport clamping', () => {
  const anchor = visiblePanelWidth(480, 310)
  for (const direction of ['ltr', 'rtl']) {
    const sign = direction === 'rtl' ? -1 : 1
    const pointerDelta = -16 * sign
    assert.equal(anchor + pointerDelta * sign, 294)
  }
})

test('keyboard shrinking acts immediately on an oversized saved history width', () => {
  assert.equal(stepPanelWidth(480, -16, 280, 310), 294)
  assert.equal(stepPanelWidth(480, 16, 280, 310), 310)
})

test('topology keyboard resizing starts at its clamped displayed size', () => {
  const area = 1000
  const savedRatio = 0.9
  assert.equal(stepPanelWidth(savedRatio * area, -area * 0.02, 280, 700), 680)
})

test('usable limits still hold and zero available space cannot become negative', () => {
  assert.equal(stepPanelWidth(280, -16, 280, 480), 280)
  assert.equal(stepPanelWidth(480, 16, 280, 480), 480)
  assert.equal(stepPanelWidth(480, -16, 280, 200), 200)
  assert.equal(stepPanelWidth(480, 16, 280, 0), 0)
  assert.equal(visiblePanelWidth(480, -10), 0)
})
