package mcpserver

// rpcKinds is the kind of every call in the node's API, NodeService: what an
// agent doing it would do, and so whether a server offers a tool for it
// (#650). A call the same tools cannot undo, such as deleting something or
// leaving the pool, is destructive; a setting that can be set back is
// admin. TestEveryNodeCallHasAKind fails on a call missing from it, so a
// new call cannot reach agents without that decision.
//
// It is what the tools are being brought to: a call with no tool yet has
// its kind here already, and some tools still offered more widely are
// named as destructive, for their user to confirm (#650).
var rpcKinds = map[string]kind{
	// The node, its peers and its settings, as they stand.
	"GetNodeInfo":                     reads,
	"WatchNodeState":                  reads,
	"WatchSharedState":                reads,
	"WatchPairedBrowsers":             reads,
	"WatchPendingActionConfirmations": reads,
	"GetLocationDiscoverySettings":    reads,
	"GetNodeLocation":                 reads,
	"GetCityTable":                    reads,
	"GetPoolLimits":                   reads,
	"GetWebAccessConfig":              reads,
	"ListNetworkInterfaces":           reads,
	"ListPeers":                       reads,
	"WatchPeers":                      reads,
	"GetBootstrapPeers":               reads,
	"GetJoinedPool":                   reads,
	"WatchJoinedPool":                 reads,

	// The pool's work, files and members.
	"ListWorkers":    reads,
	"WatchWorkers":   reads,
	"ListWorkloads":  reads,
	"GetJob":         reads,
	"ListJobs":       reads,
	"WatchJobs":      reads,
	"WatchJobEvents": reads,
	"FetchFile":      reads,
	"ListFiles":      reads,
	"ListMembers":    reads,
	"SubmitJob":      uses,
	"CancelJob":      uses,
	"StoreFile":      uses,

	// The planner and the models it may use. None of these returns a key.
	"GetModelConfig":    reads,
	"ListModelProfiles": reads,
	"GetModelLending":   reads,
	"InspectOllama":     reads,
	"ListProviders":     reads,
	"ListModels":        reads,
	"ListChats":         reads,
	"GetChat":           reads,
	"Ask":               uses,

	// Changes to the node that can be changed back.
	"SetLocationDiscoverySettings": admin,
	"SetNodeLocation":              admin,
	"DownloadCityTable":            admin,
	"SetPoolLimits":                admin,
	"SetWebAccessConfig":           admin,
	"SetBootstrapPeers":            admin,
	"ConnectPeer":                  admin,
	"SetPeerComputeTrust":          admin,
	"SetPeerComputePermissions":    admin,
	"SetModelConfig":               admin,
	"SaveModelProfile":             admin,
	"SetModelLending":              admin,
	"ResolveActionConfirmation":    destructive,
	"PullModel":                    admin,
	"CreateInvitation":             admin,
	"JoinPool":                     admin,

	// What cannot be undone.
	"RemoveCityTable":           destructive,
	"DeleteModelProfile":        destructive,
	"RemoveModel":               destructive,
	"DeleteChat":                destructive,
	"RemoveFile":                destructive,
	"RemoveMember":              destructive,
	"LeavePool":                 destructive,
	"RequestActionConfirmation": destructive,
}
