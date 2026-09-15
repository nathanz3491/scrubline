// Package meta holds every project-identifying string in one place, so that
// retargeting the project (different org, different repo, vendored into a
// subdirectory) is a find-replace here plus .goreleaser.yaml, not a restructure.
package meta

const (
	// Name is the binary name and the name used in all user-facing output.
	Name = "scrubline"

	// Org and Repo identify the upstream project.
	Org  = "nathanz3491"
	Repo = "scrubline"

	// ModulePath is the Go module path; must match go.mod.
	ModulePath = "github.com/" + Org + "/" + Repo

	// RepoURL is the project home, used in help output and error messages.
	RepoURL = "https://" + ModulePath

	// RefNamespace is the git ref namespace snapshots are written under.
	// Everything scrubline stores in a user's repository lives here.
	RefNamespace = "refs/" + Name

	// TimelineRef is the ref whose commit history is the snapshot timeline.
	TimelineRef = RefNamespace + "/timeline"

	// StateDir is the directory inside .git where scrubline keeps its index
	// cache, pending label, and lock file.
	StateDir = Name
)

// Version is overwritten at build time via -ldflags "-X .../meta.Version=x".
var Version = "dev"
