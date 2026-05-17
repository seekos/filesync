package model

type FileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	ModTime int64  `json:"mod_time"`
}

type ManifestRequest struct {
	Destination      string      `json:"destination"`
	Files            []FileEntry `json:"files"`
	DeleteExtraneous bool        `json:"delete_extraneous"`
}

type ManifestResponse struct {
	Needed []string `json:"needed"`
}

type PruneRequest struct {
	Destination string   `json:"destination"`
	Files       []string `json:"files"`
}

type UploadResponse struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
}

type CompleteRequest struct {
	Destination string `json:"destination"`
	Path        string `json:"path"`
	Mode        uint32 `json:"mode"`
	ModTime     int64  `json:"mod_time"`
	Size        int64  `json:"size"`
}

type BeginRequest struct {
	Destination string `json:"destination"`
	Path        string `json:"path"`
	Mode        uint32 `json:"mode"`
	ModTime     int64  `json:"mod_time"`
	Size        int64  `json:"size"`
}

type AbortRequest struct {
	Destination string `json:"destination"`
	Path        string `json:"path"`
}
