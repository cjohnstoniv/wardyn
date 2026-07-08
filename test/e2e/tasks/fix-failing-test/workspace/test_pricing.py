"""Tests for pricing.total — stdlib unittest (no network, no pytest needed).

Two tests FAIL with the seeded bug (the percentage cases) and three PASS as
regression guards (the discount_pct == 0 cases, where the buggy flat-subtract and
the correct percentage form coincide). A correct fix must make all five pass.
"""
import unittest

from pricing import total


class TestTotal(unittest.TestCase):
    # ── regression guards: pass with both the bug and the fix (discount 0) ──
    def test_no_discount_multi(self):
        self.assertEqual(total([10, 20, 30], 0), 60)

    def test_no_discount_empty(self):
        self.assertEqual(total([], 0), 0)

    def test_no_discount_single(self):
        self.assertEqual(total([42], 0), 42)

    # ── percentage cases: FAIL with the flat-subtract bug ──────────────────
    def test_ten_percent(self):
        # subtotal 50, 10% off -> 45.0  (bug would give 50 - 10 = 40)
        self.assertEqual(total([20, 30], 10), 45.0)

    def test_quarter_off(self):
        # subtotal 200, 25% off -> 150.0  (bug would give 200 - 25 = 175)
        self.assertEqual(total([100, 100], 25), 150.0)


if __name__ == "__main__":
    unittest.main()
