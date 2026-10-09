// gRPC FAILED_PRECONDITION: the planner cannot run with its current setup.
// Do not infer status from human-readable (potentially localized) error text.
export function needsPlannerSetup(code: number | undefined): boolean {
  return code === 9
}
