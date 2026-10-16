package main

func init() {
	for name, handler := range map[string]Handler{
		"run": cmdRun, "status": cmdStatus, "grep": cmdGrep, "steer": cmdSteer,
		"prompt": cmdPrompt, "interrupt": cmdInterrupt, "cancel": cmdCancel,
		"terminate": cmdTerminate, "resume": cmdResume, "approvals": cmdApprovals,
		"approve": cmdApprove, "deny": cmdDeny, "result": cmdResult, "ls": cmdLS,
	} {
		commandRegistry[name] = handler
	}
}
