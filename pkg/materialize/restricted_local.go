package materialize

import apischema "github.com/danieliser/agentruntime/pkg/api/schema"

func restrictedLocal(req *apischema.SessionRequest) bool {
	return req.Runtime == "local" && req.ProviderPermissions != nil && req.ProviderPermissions.Mode == "restricted"
}
