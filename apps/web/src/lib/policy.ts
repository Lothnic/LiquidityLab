import type { PolicyAction } from './api';

const actions: Record<PolicyAction, string> = {
  1: 'Wait',
  2: 'Cancel current order',
  3: 'Post small limit order',
  4: 'Post limit order',
  5: 'Cross spread with small limit order',
  6: 'Cross spread with limit order',
};

export const policyActionLabel = (action: PolicyAction) => actions[action];
