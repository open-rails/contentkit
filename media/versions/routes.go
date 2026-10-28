package versions

// Pending answers a version whose media is still being prepared: retry.
type Pending struct {
	Status string `json:"status"` // "pending"
}

const (
	// CodeNoDownload is a version without the file its kind's download serves.
	CodeNoDownload = "no_download"
)

const (
	// CodeDownloadUnavailable is a download the host's policy refused.
	CodeDownloadUnavailable = "download_unavailable"
)
