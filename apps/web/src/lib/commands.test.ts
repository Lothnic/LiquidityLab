import { describe, expect, it } from 'vitest';
import { createPlanInput, holdCommand, retryBody } from './commands';

describe('trader command safety helpers', () => {
  it('preserves decimal text exactly when forming a plan', () => {
    const plan = createPlanInput({
      symbol: 'BTC/USD', side: 'buy', quantity: '0.05000000', limit_price: '102.0100',
      deadline_ns: 900_000_000_000, max_child_quantity: '0.010000', quote_budget: '1000.000', base_balance: '1.00',
    });
    expect(plan.quantity).toBe('0.05000000');
    expect(plan.limit_price).toBe('102.0100');
    expect(plan.quote_budget).toBe('1000.000');
  });

  it('keeps the exact accepted command bytes and revision for an outcome-unknown retry', () => {
    const pending = holdCommand({
      id: 'create-1', expected_revision: 43, expected_authority_revision: 40, kind: 'create', plan_id: 'plan-1', policy: 'scheduled',
      plan: { symbol: 'BTC/USD', side: 'buy', quantity: '0.0500', limit_price: '102.00', deadline_ns: 900_000_000_000, max_child_quantity: '0.01', quote_budget: '6.00', base_balance: '0' },
    });
    expect(retryBody(pending)).toBe(pending.serialized);
    expect(retryBody(pending)).toBe(JSON.stringify(pending.command));
    expect(pending.command.expected_revision).toBe(43);
    expect(pending.command.expected_authority_revision).toBe(40);
    expect(() => { (pending.command as { expected_revision: number }).expected_revision = 44; }).toThrow();
  });
});
