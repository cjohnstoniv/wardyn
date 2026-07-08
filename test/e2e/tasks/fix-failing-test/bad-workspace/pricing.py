"""Tiny pricing helper.

total(items, discount_pct) returns the sum of item prices with a percentage
discount applied.
"""


def total(items, discount_pct):
    """Sum ``items`` and apply a ``discount_pct`` percent discount.

    BUG: the discount is subtracted as a flat amount instead of applied as a
    percentage of the subtotal. It happens to be correct when discount_pct is 0
    (or when the subtotal is exactly 100), which is why the regression tests pass
    but the percentage tests fail.
    """
    subtotal = sum(items)
    return subtotal - discount_pct
