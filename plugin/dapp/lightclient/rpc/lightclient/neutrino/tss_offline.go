package neutrino

// Exported aliases of the TSS storage contract, for the offline backup tool
// (cmd/rgb-tss-tool). The tool runs outside a node and must address exactly the
// same bucket and keys as the node itself; aliasing the unexported constants
// keeps a single source of truth instead of duplicating the literals.
const (
	TSSBucketName       = tssBucketName
	TSSDKGResultKey     = dkgResultKey
	TSSRefreshResultKey = refreshResultKey
)
