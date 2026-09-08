package version

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
}
