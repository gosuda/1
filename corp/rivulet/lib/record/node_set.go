package record

type Node struct {
	InitAddress    string `json:"init_address"`    // Initial address of the node
	ClusterAddress string `json:"cluster_address"` // Cluster address of the node
	IsActive       bool   `json:"is_active"`       // Indicates if the node is active
}
