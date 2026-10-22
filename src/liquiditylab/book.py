"""Conservative replay book with persistent hypothetical consumption debt."""

from .contracts import ZERO, decimal


class Book:
    def __init__(self):
        self.bids = {}
        self.asks = {}
        self.debt = {"bids": {}, "asks": {}}
        self.valid = False

    def apply(self, event):
        kind = event.get("kind", event.get("type"))
        if kind == "gap" or not event.get("valid", True):
            self.valid = False
            return
        if kind not in ("snapshot", "book"):
            return
        if kind == "snapshot":
            # A recovery snapshot does not prove our simulated liquidity replenished.
            self.bids.clear()
            self.asks.clear()
            self.valid = True
        elif not self.valid:
            return
        for side in ("bids", "asks"):
            levels = getattr(self, side)
            for p, q in event.get(side, []):
                price, quantity = decimal(p), decimal(q)
                if price <= 0 or quantity < 0:
                    raise ValueError("invalid book level")
                if quantity == 0:
                    levels.pop(price, None)
                    self.debt[side].pop(price, None)
                else:
                    levels[price] = quantity
        if not self.bids or not self.asks or self.best_bid >= self.best_ask:
            self.valid = False

    @property
    def best_bid(self):
        return max(self.bids) if self.bids else ZERO

    @property
    def best_ask(self):
        return min(self.asks) if self.asks else ZERO

    @property
    def mid(self):
        return (self.best_bid + self.best_ask) / 2

    def available(self, side, price):
        return max(getattr(self, side).get(price, ZERO) - self.debt[side].get(price, ZERO), ZERO)

    def consume(self, side, price, quantity):
        self.debt[side][price] = self.debt[side].get(price, ZERO) + quantity

    def visible(self, side):
        return sum((self.available(side, p) for p in getattr(self, side)), ZERO)
