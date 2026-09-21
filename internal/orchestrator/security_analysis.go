package orchestrator

// AnalyzeSecurityPaths applies the same control-file and security-marker
// classification used by a live review. It is exported for read-only planning
// so the CLI can explain a targeted security pass without starting a run.
func AnalyzeSecurityPaths(paths, markers []string) (controlFiles, sensitivePaths []string) {
	controlFiles = changedControlFiles(paths)
	sensitivePaths = mergePaths(controlFiles, securitySensitivePaths(paths, markers))
	return controlFiles, sensitivePaths
}
