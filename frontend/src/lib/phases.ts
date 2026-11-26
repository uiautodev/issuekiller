export type PhaseClass = 'idle' | 'working' | 'waiting' | 'blocked' | 'done';

export interface PhaseMeta {
	key: string;
	cls: PhaseClass;
	step: number;
}

// Mirrors phaseNeedsUser in web.go: the ball is in the approver's court.
export const WAIT_PHASES = ['awaiting_approval', 'pr_open', 'blocked'];

export function needsMe(phase: string): boolean {
	return WAIT_PHASES.includes(phase);
}

export const PHASES: Record<string, PhaseMeta> = {
	new: { key: 'phase.new', cls: 'idle', step: 1 },
	'': { key: 'phase.new', cls: 'idle', step: 1 },
	planning: { key: 'phase.planning', cls: 'working', step: 1 },
	awaiting_approval: { key: 'phase.awaiting', cls: 'waiting', step: 2 },
	implementing: { key: 'phase.implementing', cls: 'working', step: 3 },
	blocked: { key: 'phase.blocked', cls: 'blocked', step: 3 },
	pr_open: { key: 'phase.prOpen', cls: 'waiting', step: 5 },
	merging: { key: 'phase.merging', cls: 'working', step: 6 },
	done: { key: 'phase.done', cls: 'done', step: 6 },
	cancelled: { key: 'phase.cancelled', cls: 'idle', step: 0 },
	// Pull-request reviews reuse the board but not the issue phase machine.
	// The rail has no review stage, so a running review sits at the PR/checks
	// step and a finished one fills it.
	reviewing: { key: 'phase.reviewing', cls: 'working', step: 5 },
	reviewed: { key: 'phase.reviewed', cls: 'done', step: 6 },
	reviewfailed: { key: 'phase.reviewFailed', cls: 'blocked', step: 5 }
};

export function phaseMeta(phase: string): PhaseMeta {
	return PHASES[phase] || PHASES.new;
}

// Finished rows include successful and failed reviews: both are history, not
// work the board must keep in front of the user.
export function isFinished(phase: string): boolean {
	return ['done', 'cancelled', 'reviewed', 'reviewfailed'].includes(phase);
}
