package transfer

type TransferRecord struct {
	TransferID  string `json:"transferId"`
	Direction   string `json:"direction"`
	PeerNode    string `json:"peerNode"`
	FileName    string `json:"fileName"`
	FileSize    int64  `json:"fileSize"`
	MimeType    string `json:"mimeType,omitempty"`
	Checksum    string `json:"checksum,omitempty"`
	Status      string `json:"status"`
	LocalPath   string `json:"localPath,omitempty"`
	Bucket      string `json:"bucket"`
	CreatedAt   int64  `json:"createdAt"`
	CompletedAt int64  `json:"completedAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

type SendFileRequest struct {
	TargetNode string
	FilePath   string
	MimeType   string
}

type SendFileResult struct {
	TransferID string
	Bucket     string
	Size       int64
	Checksum   string
}

type PullResult struct {
	TransferID string
	FilePath   string
	Size       int64
	Checksum   string
}

type TransferInfo struct {
	TransferID  string `json:"transferId"`
	Direction   string `json:"direction"`
	PeerNode    string `json:"peerNode"`
	FileName    string `json:"fileName"`
	FileSize    int64  `json:"fileSize"`
	MimeType    string `json:"mimeType,omitempty"`
	Checksum    string `json:"checksum,omitempty"`
	Status      string `json:"status"`
	LocalPath   string `json:"localPath,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	CompletedAt int64  `json:"completedAt,omitempty"`
}

type TransferConfig struct {
	TransferDir string
	MaxFileSize int64
	TTL         string
}
