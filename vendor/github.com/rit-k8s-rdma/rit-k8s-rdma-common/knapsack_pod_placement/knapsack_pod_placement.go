package knapsack_pod_placement

import (
	"log"

	"github.com/rit-k8s-rdma/rit-k8s-rdma-common/rdma_hardware_info"
)

//Structure that defines the parameters that an RDMA interface can have in a
//	pod YAML file.
type RdmaInterfaceRequest struct {
	MinTxRate uint `json:"min_tx_rate"`
	MaxTxRate uint `json:"max_tx_rate"`
}

//PlacePod takes in a list of required RDMA interfaces and a list of PFs
//	available on a node and determines whether the unused bandwidth and
//	VFs on that node can satisfy all of the required RDMA interfaces in
//	the list. It returns boolean flag indicating whether the request can
//	be satisfied, along with a list of indicies representing how the
//	required interfaces can be used to satisfy it (if the request is not
//	satisfiable, this list will be empty).
//
//	Pod placement is currently done using a straightforward iterative
//	backtracking algorithm.
func PlacePod(requested_interfaces []RdmaInterfaceRequest,
	pfs_available []rdma_hardware_info.PF,
	debug_logging bool) ([]int, bool) {

	//if no interfaces are required
	if len(requested_interfaces) <= 0 {
		//request is trivially satisfiable
		return []int{}, true
	}

	//index of the current requested item being processed in the 'requested_interfaces' list
	var current_requested int = 0
	//list of which PF each of the requested interfaces can be "placed" on
	//	to satisfy the overall request. -1 means a requested interface
	//	has not been placed yet.
	var placements = make([]int, len(requested_interfaces))
	for index, _ := range placements {
		placements[index] = -1
	}
	//flag indicating whether we were able to satisfy the request
	var all_interfaces_sucessfully_placed bool = false

	//while we haven't satisfied the request or run out of placement options
	for {
		if debug_logging {
			log.Println("Outer loop iteration:")
			log.Println("\tcurrent_requested=", current_requested, " (value=", requested_interfaces[current_requested].MinTxRate, ")")
		}

		//move to next placement for current item
		for placements[current_requested]++; placements[current_requested] < len(pfs_available); placements[current_requested]++ {
			var cur_pf *rdma_hardware_info.PF = &(pfs_available[placements[current_requested]])
			//if the current pf can fit the current requested interface
			if ((*cur_pf).CapacityVFs - (*cur_pf).UsedVFs) > 0 {
				if (int((*cur_pf).CapacityTxRate) - int((*cur_pf).UsedTxRate)) >= int(requested_interfaces[current_requested].MinTxRate) {
					//add the current interface's bandwidth to the pf's used bw
					(*cur_pf).UsedTxRate += requested_interfaces[current_requested].MinTxRate
					(*cur_pf).UsedVFs += 1
					break
				}
			}
		}

		if debug_logging {
			log.Println("\tplacement=", placements[current_requested])
		}

		//if there was no valid placement for the current item,
		//	try to backtrack to the previous one
		if placements[current_requested] >= len(pfs_available) {
			//if the current item was item #0
			if current_requested == 0 {
				//then there is no backtracking left to do,
				//	the request cannot be satisfied.
				all_interfaces_sucessfully_placed = false
				break
				//otherwise, if we can still backtrack to the previous item
			} else {
				//reset placement of current item
				placements[current_requested] = -1
				//move index to previous item
				current_requested--
				//subtract the bw and vf of previous item from the pf it was allocated to
				pfs_available[placements[current_requested]].UsedTxRate -= requested_interfaces[current_requested].MinTxRate
				pfs_available[placements[current_requested]].UsedVFs -= 1
				//try the next placement for the previous item
				continue
			}

			//otherwise, there was a valid placement for the current item
		} else {
			//if current item was the last one
			if current_requested == (len(requested_interfaces) - 1) {
				//we have satisfied the whole request
				all_interfaces_sucessfully_placed = true
				break

				//otherwise
			} else {
				//move to the next item
				current_requested++
				continue
			}
		}
	}

	//if the request could be satisfied
	if all_interfaces_sucessfully_placed {
		//return the allocation that satisfied it
		return placements, true
	}

	//request could not be satisfied, just return empty allocation
	return []int{}, false
}
