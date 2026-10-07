// Package skills carries the skills that teach an AI agent to use Sisyphus,
// so that the daemon can hand them out: to an agent that asks for one as a
// resource, and to a directory where an agent keeps its skills.
package skills

import "embed"

// Files holds each skill in a directory of its name.
//
//go:embed sisyphus
var Files embed.FS
