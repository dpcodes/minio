/*
 * Minio Cloud Storage, (C) 2014-2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"reflect"
	"sort"
	"sync"
	"time"
)

// localAdminClient - represents admin operation to be executed locally.
type localAdminClient struct {
}

// remoteAdminClient - represents admin operation to be executed
// remotely, via RPC.
type remoteAdminClient struct {
	*AuthRPCClient
}

// adminCmdRunner - abstracts local and remote execution of admin
// commands like service stop and service restart.
type adminCmdRunner interface {
	Restart() error
	ListLocks(bucket, prefix string, duration time.Duration) ([]VolumeLockInfo, error)
	ReInitDisks() error
	Uptime() (time.Duration, error)
	GetConfig() ([]byte, error)
}

// Restart - Sends a message over channel to the go-routine
// responsible for restarting the process.
func (lc localAdminClient) Restart() error {
	globalServiceSignalCh <- serviceRestart
	return nil
}

// ListLocks - Fetches lock information from local lock instrumentation.
func (lc localAdminClient) ListLocks(bucket, prefix string, duration time.Duration) ([]VolumeLockInfo, error) {
	return listLocksInfo(bucket, prefix, duration), nil
}

// Restart - Sends restart command to remote server via RPC.
func (rc remoteAdminClient) Restart() error {
	args := AuthRPCArgs{}
	reply := AuthRPCReply{}
	return rc.Call("Admin.Restart", &args, &reply)
}

// ListLocks - Sends list locks command to remote server via RPC.
func (rc remoteAdminClient) ListLocks(bucket, prefix string, duration time.Duration) ([]VolumeLockInfo, error) {
	listArgs := ListLocksQuery{
		bucket:   bucket,
		prefix:   prefix,
		duration: duration,
	}
	var reply ListLocksReply
	if err := rc.Call("Admin.ListLocks", &listArgs, &reply); err != nil {
		return nil, err
	}
	return reply.volLocks, nil
}

// ReInitDisks - There is nothing to do here, heal format REST API
// handler has already formatted and reinitialized the local disks.
func (lc localAdminClient) ReInitDisks() error {
	return nil
}

// ReInitDisks - Signals peers via RPC to reinitialize their disks and
// object layer.
func (rc remoteAdminClient) ReInitDisks() error {
	args := AuthRPCArgs{}
	reply := AuthRPCReply{}
	return rc.Call("Admin.ReInitDisks", &args, &reply)
}

// Uptime - Returns the uptime of this server. Timestamp is taken
// after object layer is initialized.
func (lc localAdminClient) Uptime() (time.Duration, error) {
	if globalBootTime.IsZero() {
		return time.Duration(0), errServerNotInitialized
	}

	return time.Now().UTC().Sub(globalBootTime), nil
}

// Uptime - returns the uptime of the server to which the RPC call is made.
func (rc remoteAdminClient) Uptime() (time.Duration, error) {
	args := AuthRPCArgs{}
	reply := UptimeReply{}
	err := rc.Call("Admin.Uptime", &args, &reply)
	if err != nil {
		return time.Duration(0), err
	}

	return reply.Uptime, nil
}

// GetConfig - returns config.json of the local server.
func (lc localAdminClient) GetConfig() ([]byte, error) {
	if serverConfig == nil {
		return nil, errors.New("config not present")
	}

	return json.Marshal(serverConfig)
}

// GetConfig - returns config.json of the remote server.
func (rc remoteAdminClient) GetConfig() ([]byte, error) {
	args := AuthRPCArgs{}
	reply := ConfigReply{}
	if err := rc.Call("Admin.GetConfig", &args, &reply); err != nil {
		return nil, err
	}
	return reply.Config, nil
}

// adminPeer - represents an entity that implements Restart methods.
type adminPeer struct {
	addr      string
	cmdRunner adminCmdRunner
}

// type alias for a collection of adminPeer.
type adminPeers []adminPeer

// makeAdminPeers - helper function to construct a collection of adminPeer.
func makeAdminPeers(eps []*url.URL) adminPeers {
	var servicePeers []adminPeer

	// map to store peers that are already added to ret
	seenAddr := make(map[string]bool)

	// add local (self) as peer in the array
	servicePeers = append(servicePeers, adminPeer{
		globalMinioAddr,
		localAdminClient{},
	})
	seenAddr[globalMinioAddr] = true

	serverCred := serverConfig.GetCredential()
	// iterate over endpoints to find new remote peers and add
	// them to ret.
	for _, ep := range eps {
		if ep.Host == "" {
			continue
		}

		// Check if the remote host has been added already
		if !seenAddr[ep.Host] {
			cfg := authConfig{
				accessKey:       serverCred.AccessKey,
				secretKey:       serverCred.SecretKey,
				serverAddr:      ep.Host,
				secureConn:      globalIsSSL,
				serviceEndpoint: path.Join(minioReservedBucketPath, adminPath),
				serviceName:     "Admin",
			}

			servicePeers = append(servicePeers, adminPeer{
				addr:      ep.Host,
				cmdRunner: &remoteAdminClient{newAuthRPCClient(cfg)},
			})
			seenAddr[ep.Host] = true
		}
	}

	return servicePeers
}

// Initialize global adminPeer collection.
func initGlobalAdminPeers(eps []*url.URL) {
	globalAdminPeers = makeAdminPeers(eps)
}

// invokeServiceCmd - Invoke Restart command.
func invokeServiceCmd(cp adminPeer, cmd serviceSignal) (err error) {
	switch cmd {
	case serviceRestart:
		err = cp.cmdRunner.Restart()
	}
	return err
}

// sendServiceCmd - Invoke Restart command on remote peers
// adminPeer followed by on the local peer.
func sendServiceCmd(cps adminPeers, cmd serviceSignal) {
	// Send service command like stop or restart to all remote nodes and finally run on local node.
	errs := make([]error, len(cps))
	var wg sync.WaitGroup
	remotePeers := cps[1:]
	for i := range remotePeers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// we use idx+1 because remotePeers slice is 1 position shifted w.r.t cps
			errs[idx+1] = invokeServiceCmd(remotePeers[idx], cmd)
		}(i)
	}
	wg.Wait()
	errs[0] = invokeServiceCmd(cps[0], cmd)
}

// listPeerLocksInfo - fetch list of locks held on the given bucket,
// matching prefix held longer than duration from all peer servers.
func listPeerLocksInfo(peers adminPeers, bucket, prefix string, duration time.Duration) ([]VolumeLockInfo, error) {
	// Used to aggregate volume lock information from all nodes.
	allLocks := make([][]VolumeLockInfo, len(peers))
	errs := make([]error, len(peers))
	var wg sync.WaitGroup
	localPeer := peers[0]
	remotePeers := peers[1:]
	for i, remotePeer := range remotePeers {
		wg.Add(1)
		go func(idx int, remotePeer adminPeer) {
			defer wg.Done()
			// `remotePeers` is right-shifted by one position relative to `peers`
			allLocks[idx], errs[idx] = remotePeer.cmdRunner.ListLocks(bucket, prefix, duration)
		}(i+1, remotePeer)
	}
	wg.Wait()
	allLocks[0], errs[0] = localPeer.cmdRunner.ListLocks(bucket, prefix, duration)

	// Summarizing errors received for ListLocks RPC across all
	// nodes.  N B the possible unavailability of quorum in errors
	// applies only to distributed setup.
	errCount, err := reduceErrs(errs, []error{})
	if err != nil {
		if errCount >= (len(peers)/2 + 1) {
			return nil, err
		}
		return nil, InsufficientReadQuorum{}
	}

	// Group lock information across nodes by (bucket, object)
	// pair. For readability only.
	paramLockMap := make(map[nsParam][]VolumeLockInfo)
	for _, nodeLocks := range allLocks {
		for _, lockInfo := range nodeLocks {
			param := nsParam{
				volume: lockInfo.Bucket,
				path:   lockInfo.Object,
			}
			paramLockMap[param] = append(paramLockMap[param], lockInfo)
		}
	}
	groupedLockInfos := []VolumeLockInfo{}
	for _, volLocks := range paramLockMap {
		groupedLockInfos = append(groupedLockInfos, volLocks...)
	}
	return groupedLockInfos, nil
}

// reInitPeerDisks - reinitialize disks and object layer on peer servers to use the new format.
func reInitPeerDisks(peers adminPeers) error {
	errs := make([]error, len(peers))

	// Send ReInitDisks RPC call to all nodes.
	// for local adminPeer this is a no-op.
	wg := sync.WaitGroup{}
	for i, peer := range peers {
		wg.Add(1)
		go func(idx int, peer adminPeer) {
			defer wg.Done()
			errs[idx] = peer.cmdRunner.ReInitDisks()
		}(i, peer)
	}
	wg.Wait()
	return nil
}

// uptimeSlice - used to sort uptimes in chronological order.
type uptimeSlice []struct {
	err    error
	uptime time.Duration
}

func (ts uptimeSlice) Len() int {
	return len(ts)
}

func (ts uptimeSlice) Less(i, j int) bool {
	return ts[i].uptime < ts[j].uptime
}

func (ts uptimeSlice) Swap(i, j int) {
	ts[i], ts[j] = ts[j], ts[i]
}

// getPeerUptimes - returns the uptime since the last time read quorum
// was established on success. Otherwise returns errXLReadQuorum.
func getPeerUptimes(peers adminPeers) (time.Duration, error) {
	// In a single node Erasure or FS backend setup the uptime of
	// the setup is the uptime of the single minio server
	// instance.
	if !globalIsDistXL {
		return time.Now().UTC().Sub(globalBootTime), nil
	}

	uptimes := make(uptimeSlice, len(peers))

	// Get up time of all servers.
	wg := sync.WaitGroup{}
	for i, peer := range peers {
		wg.Add(1)
		go func(idx int, peer adminPeer) {
			defer wg.Done()
			uptimes[idx].uptime, uptimes[idx].err = peer.cmdRunner.Uptime()
		}(i, peer)
	}
	wg.Wait()

	// Sort uptimes in chronological order.
	sort.Sort(uptimes)

	// Pick the readQuorum'th uptime in chronological order. i.e,
	// the time at which read quorum was (re-)established.
	readQuorum := len(uptimes) / 2
	validCount := 0
	latestUptime := time.Duration(0)
	for _, uptime := range uptimes {
		if uptime.err != nil {
			errorIf(uptime.err, "Unable to fetch uptime")
			continue
		}

		validCount++
		if validCount >= readQuorum {
			latestUptime = uptime.uptime
			break
		}
	}

	// Less than readQuorum "Admin.Uptime" RPC call returned
	// successfully, so read-quorum unavailable.
	if validCount < readQuorum {
		return time.Duration(0), InsufficientReadQuorum{}
	}

	return latestUptime, nil
}

// getPeerConfig - Fetches config.json from all nodes in the setup and
// returns the one that occurs in a majority of them.
func getPeerConfig(peers adminPeers) ([]byte, error) {
	if !globalIsDistXL {
		return peers[0].cmdRunner.GetConfig()
	}

	errs := make([]error, len(peers))
	configs := make([][]byte, len(peers))

	// Get config from all servers.
	wg := sync.WaitGroup{}
	for i, peer := range peers {
		wg.Add(1)
		go func(idx int, peer adminPeer) {
			defer wg.Done()
			configs[idx], errs[idx] = peer.cmdRunner.GetConfig()
		}(i, peer)
	}
	wg.Wait()

	// Find the maximally occurring config among peers in a
	// distributed setup.

	serverConfigs := make([]serverConfigV13, len(peers))
	for i, configBytes := range configs {
		if errs[i] != nil {
			continue
		}

		// Unmarshal the received config files.
		err := json.Unmarshal(configBytes, &serverConfigs[i])
		if err != nil {
			errorIf(err, "Failed to unmarshal serverConfig from ", peers[i].addr)
			return nil, err
		}
	}

	configJSON, err := getValidServerConfig(serverConfigs, errs)
	if err != nil {
		errorIf(err, "Unable to find a valid server config")
		return nil, traceError(err)
	}

	// Return the config.json that was present quorum or more
	// number of disks.
	return json.Marshal(configJSON)
}

// getValidServerConfig - finds the server config that is present in
// quorum or more number of servers.
func getValidServerConfig(serverConfigs []serverConfigV13, errs []error) (serverConfigV13, error) {
	// majority-based quorum
	quorum := len(serverConfigs)/2 + 1

	// Count the number of disks a config.json was found in.
	configCounter := make([]int, len(serverConfigs))

	// We group equal serverConfigs by the lowest index of the
	// same value;  e.g, let us take the following serverConfigs
	// in a 4-node setup,
	// serverConfigs == [c1, c2, c1, c1]
	// configCounter == [3, 1, 0, 0]
	// c1, c2 are the only distinct values that appear.  c1 is
	// identified by 0, the lowest index it appears in and c2 is
	// identified by 1. So, we need to find the number of times
	// each of these distinct values occur.

	// Invariants:

	// 1. At the beginning of the i-th iteration, the number of
	// unique configurations seen so far is equal to the number of
	// non-zero counter values in config[:i].

	// 2. At the beginning of the i-th iteration, the sum of
	// elements of configCounter[:i] is equal to the number of
	// non-error configurations seen so far.

	// For each of the serverConfig ...
	for i := range serverConfigs {
		// Skip nodes where getConfig failed.
		if errs[i] != nil {
			continue
		}
		// Check if it is equal to any of the configurations
		// seen so far. If j == i is reached then we have an
		// unseen configuration.
		for j := 0; j <= i; j++ {
			if j < i && configCounter[j] == 0 {
				// serverConfigs[j] is known to be
				// equal to a value that was already
				// seen. See example above for
				// clarity.
				continue
			} else if j < i && reflect.DeepEqual(serverConfigs[i], serverConfigs[j]) {
				// serverConfigs[i] is equal to
				// serverConfigs[j], update
				// serverConfigs[j]'s counter since it
				// is the lower index.
				configCounter[j]++
				break
			} else if j == i {
				// serverConfigs[i] is equal to no
				// other value seen before. It is
				// unique so far.
				configCounter[i] = 1
				break
			} // else invariants specified above are violated.
		}
	}

	// We find the maximally occurring server config and check if
	// there is quorum.
	var configJSON serverConfigV13
	maxOccurrence := 0
	for i, count := range configCounter {
		if maxOccurrence < count {
			maxOccurrence = count
			configJSON = serverConfigs[i]
		}
	}

	// If quorum nodes don't agree.
	if maxOccurrence < quorum {
		return serverConfigV13{}, errXLWriteQuorum
	}

	return configJSON, nil
}
