import type { Command, PlanInput } from './api';

export type PendingCommand = Readonly<{ command: Readonly<Command>; serialized: string }>;

export function createPlanInput(input: PlanInput): PlanInput {
  // Keep financial inputs as the exact strings typed by the operator. The API
  // parses them as decimals; the browser must not round them through Number.
  return { ...input };
}

export function holdCommand(command: Command): PendingCommand {
  const frozen = Object.freeze({ ...command, ...(command.plan ? { plan: Object.freeze({ ...command.plan }) } : {}) });
  return Object.freeze({ command: frozen, serialized: JSON.stringify(frozen) });
}

export function retryBody(pending: PendingCommand): string {
  return pending.serialized;
}
