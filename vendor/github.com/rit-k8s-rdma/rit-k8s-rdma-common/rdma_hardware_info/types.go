package rdma_hardware_info

//PF stands for 'Physical Function' and is a representation of a PF
//that exists on a machine. The PF in this case is directly related
//to one that is SRIOV enabled. It contains metadata about that device.
type PF struct {
	Name           string `json:"name"`
	UsedTxRate     uint   `json:"used_tx_rate"`
	CapacityTxRate uint   `json:"capacity_tx_rate"`
	UsedVFs        uint   `json:"used_vfs"`
	CapacityVFs    uint   `json:"capacity_vfs"`
	VFs            []*VF  `json:"vfs"`
}

//FindAssociatedMac finds vf of mac if exists
func (pf *PF) FindAssociatedMac(mac string) *VF {
	for _, vf := range pf.VFs {
		if vf.MAC == mac {
			return vf
		}
	}
	return nil
}

//VF stands for 'Virtual Function' and is a representation of VF's
//that exist under an SRIOV enabled device. It includes information
//about what the current configuration of the VF is.
type VF struct {
	VFNumber   uint   `json:"vf"`
	MAC        string `json:"mac"`
	VLAN       uint   `json:"vlan"`
	QoS        uint   `json:"qos"`
	VLanProto  string `json:"vlan_proto"`
	SpoofCheck string `json:"spoof_check"`
	Trust      string `json:"trust"`
	LinkState  string `json:"link_state"`
	MinTxRate  uint   `json:"min_tx_rate"`
	MaxTxRate  uint   `json:"max_tx_rate"`
	VGTPlus    string `json:"vgt_plus"`
	RateGroup  uint   `json:"rate_group"`
	Allocated  bool   `json:"allocated"`
}
