package main

/*
------------------
Build Instructions
------------------
From the mesos_http_api folder:
 gofmt -w src/example.com/project/module/mesos_client_service/mesos_client_service.go
 go build -o mesos_http_api src/example.com/project/module/mesos_client_service/mesos_client_service.go
 ./mesos_client_service --mip 52.205.254.68 --mport 5050 --mapi "/state" -sip 172.31.34.94


 CAVEAT: Can only be run from EC2 as mesos registers with private IP (I have
 configured it that way); which means that the instances need to be in the
 same DC.
 This will be required when a non-leader instance is queried for state data;
 which gives us info about the elected leader, which we then we query for
 state data
*/

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type MesosSlaveAttributes struct {
	// Krish's NOTE: This struct is hardcoded with the attrs set with the
	// mesos-agent cloud-config, located at
	// ./coreosuserdata/coreos_user_data.go
	Host         string `json:"host,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	PublicIP     string `json:"publicip,omitempty"`
	Rack         string `json:"rack,omitempty"`
	PrivateIP    string `json:"privateip"`
}

type MesosSlave struct {
	IsActive       bool                 `json:"active"`
	MesosPid       string               `json:"pid"`
	Attributes     MesosSlaveAttributes `json:"attributes"`
	RegisteredTime float64              `json:"registered_time"`
}

type MesosState struct {
	/*
	   Krish's NOTE: Many fields are actually present, & these are in a flux;
	   I am counting the below fields to not change in the near
	   future for the logic to work. This is the stop gap arrangement till a
	   production ready mesos HTTP API is released by the community!
	*/
	// elected_time denotes when this mesos instance was elected master; field
	// exclusive to master; Eg. "elected_time": 1458344004.38701
	ElectedTime float64 `json:"elected_time,omitempty"`
	// leader denotes the current cluster master; field present in all
	// instances; Eg. "leader": "master@172.31.43.147:5050",
	Leader string `json:"leader"`
	// pid denotes the current members' mesos-pid; field present in all
	// instances; pid == leader in master;
	// Eg. "pid": "master@172.31.40.34:5050"
	Pid string `json:"pid"`
	// slaves denotes the list of slaves/agents currently managed by this
	// cluster of mesos instances; field exclusive to master
	Slaves []MesosSlave `json:"slaves"`
}

func queryMesosState(url string) (*MesosState, error) {
	fmt.Printf("Querying mesos endpoint @:%s\n", url)
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 300 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
	}
	httpReq, err := http.NewRequest("POST", url, strings.NewReader(""))
	if err != nil {
		// TODO(Krish): handle panic(err) in code
		panic(err)
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		// TODO(Krish): handle panic(err) in code
		panic(err)
	}
	defer httpResp.Body.Close()
	fmt.Printf("HTTP Response Code: '%s'\n", httpResp.Status)
	mesosState := new(MesosState)
	err = json.NewDecoder(httpResp.Body).Decode(mesosState)
	if err != nil {
		return nil, err
	}
	return mesosState, nil
}

func isSlaveRegistered(mesosHostPort, mesosApiEndpoint, agentIP string) bool {
	url := fmt.Sprintf("http://%s/%s", mesosHostPort, mesosApiEndpoint)
	// query the mesos endpoint to get the data
	stateData, err := queryMesosState(url)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	//fmt.Printf("elected_time: '%f'\n", stateData.ElectedTime)
	//fmt.Printf("leader: '%s'\n", stateData.Leader)
	//fmt.Printf("pid: '%s'\n", stateData.Pid)
	if stateData.ElectedTime == 0.0 && stateData.Leader != stateData.Pid {
		//fmt.Printf("Found a non-leader member of mesos cluster\n")
		// query leader; eg."master@172.31.43.147:5050",
		newHostPort := strings.Split(stateData.Leader, "@")[1]
		url = fmt.Sprintf("http://%s/%s", newHostPort, mesosApiEndpoint)
		//fmt.Printf("Querying discovered mesos leader @'%s'\n", url)
		stateData, err = queryMesosState(url)
	} else if stateData.ElectedTime != 0.0 &&
		stateData.Leader == stateData.Pid {
		//fmt.Printf("Found a leader of mesos cluster!\n")
	}
	// check if the slave is registered
	for _, slave := range stateData.Slaves {
		if slave.Attributes.PrivateIP == agentIP {
			return true
		}
	}
	return false
}

func asyncQueryRegistration(mesosHostPort, mesosApiEndpoint, agentIP string,
	out chan<- bool, done <-chan bool) {
	var exitFor bool
	exitFor = false

	for {
		// check whether slave is registered every 15secs
		isRegistered := isSlaveRegistered(mesosHostPort, mesosApiEndpoint,
			agentIP)
		if isRegistered == true {
			out <- isRegistered
		}
		select {
		case <-done:
			fmt.Printf("Exiting async loop....")
			// signal to stop the routine, close the channels and exit
			close(out)
			// break select
			exitFor = true
			break
		case <-time.After(time.Second * 15):
			// continue for, re-query
			continue
		}

		if exitFor == true {
			//break for
			break
		}
	}
}

func waitForMesosSlaveRegistration(mip, mport, mapi, sip string) bool {
	// check for a maximum of 5 mins to see if slave has registered
	// Krish's NOTE: docker pull is slow sometimes in the cloud
	var isRegistered bool
	stateChan := make(chan bool, 1)
	doneChan := make(chan bool, 1)

	isRegistered = false
	mesosHostPort := fmt.Sprintf("%s:%s", mip, mport)

	go asyncQueryRegistration(mesosHostPort, mapi, sip, stateChan, doneChan)

	select {
	case isRegistered = <-stateChan:
		fmt.Printf("Found a slave registered with IP: '%s'\n", sip)
	case <-time.After(time.Second * 90):
		fmt.Printf("Couldn't find mesos slave after '%s' seconds\n", "90")
	}
	doneChan <- true
	close(doneChan)
	return isRegistered
}

func main() {
	var mip, mport, mapi, sip string
	flag.StringVar(&mip, "mip", "",
		"mesos instance ip; Eg. 172.x.y.z; does not need to be mesos cluster leader")
	flag.StringVar(&mport, "mport", "5050", "mesos instance port; Eg. 5050")
	flag.StringVar(&mapi, "mapi", "/state",
		"url path to use for querying mesos state; Eg. '/state'")
	flag.StringVar(&sip, "sip", "",
		"slave private ip to wait for; Eg. 172.x.y.z")
	flag.Parse()
	// TODO(Krish): args sanity!
	slaveExists := waitForMesosSlaveRegistration(mip, mport, mapi, sip)
	fmt.Println("Slave Exists: ", slaveExists)
}

