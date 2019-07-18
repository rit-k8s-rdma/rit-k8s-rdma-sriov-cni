package rdma_hardware_info

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
)

// QueryNode performs an http request to the DaemonSet running on a node in
//	order to get the list of RDMA hardware resources available on that
//	node. Returns an empty list of PFs and an error if anything goes
//	wrong.
func QueryNode(node_address string, port string, timeout_ms int) ([]PF, error) {
	//setup a client to query the node
	http_client := http.Client{
		Timeout: time.Duration(time.Duration(timeout_ms) * time.Millisecond),
	}

	//perform http request
	resp, err := http_client.Get(fmt.Sprintf("http://%s:%s/%s", node_address, port, RdmaInfoUrl))
	if err != nil {
		return []PF{}, err
	}

	//read response
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return []PF{}, err
	}

	//deserialize response into a list of PF objects
	var pfs []PF
	err = json.Unmarshal(data, &pfs)
	if err != nil {
		return []PF{}, err
	}

	//return the list of PF objects from the node
	return pfs, nil
}
