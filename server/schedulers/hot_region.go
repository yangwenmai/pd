// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/schedule"
	log "github.com/sirupsen/logrus"
)

func init() {
	schedule.RegisterScheduler("hot-region", func(opController *schedule.OperatorController, args []string) (schedule.Scheduler, error) {
		return newBalanceHotRegionsScheduler(opController), nil
	})
	// FIXME: remove this two schedule after the balance test move in schedulers package
	schedule.RegisterScheduler("hot-write-region", func(opController *schedule.OperatorController, args []string) (schedule.Scheduler, error) {
		return newBalanceHotWriteRegionsScheduler(opController), nil
	})
	schedule.RegisterScheduler("hot-read-region", func(opController *schedule.OperatorController, args []string) (schedule.Scheduler, error) {
		return newBalanceHotReadRegionsScheduler(opController), nil
	})
}

const (
	hotRegionLimitFactor      = 0.75
	storeHotRegionsDefaultLen = 100
	hotRegionScheduleFactor   = 0.9
)

// BalanceType : the perspective of balance
type BalanceType int

const (
	hotWriteRegionBalance BalanceType = iota
	hotReadRegionBalance
)

type storeStatistics struct {
	readStatAsLeader  core.StoreHotRegionsStat
	writeStatAsPeer   core.StoreHotRegionsStat
	writeStatAsLeader core.StoreHotRegionsStat
}

func newStoreStaticstics() *storeStatistics {
	return &storeStatistics{
		readStatAsLeader:  make(core.StoreHotRegionsStat),
		writeStatAsLeader: make(core.StoreHotRegionsStat),
		writeStatAsPeer:   make(core.StoreHotRegionsStat),
	}
}

type balanceHotRegionsScheduler struct {
	*baseScheduler
	sync.RWMutex
	limit uint64
	types []BalanceType

	// store id -> hot regions statistics as the role of leader
	stats *storeStatistics
	r     *rand.Rand
}

func newBalanceHotRegionsScheduler(opController *schedule.OperatorController) *balanceHotRegionsScheduler {
	base := newBaseScheduler(opController)
	return &balanceHotRegionsScheduler{
		baseScheduler: base,
		limit:         1,
		stats:         newStoreStaticstics(),
		types:         []BalanceType{hotWriteRegionBalance, hotReadRegionBalance},
		r:             rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func newBalanceHotReadRegionsScheduler(opController *schedule.OperatorController) *balanceHotRegionsScheduler {
	base := newBaseScheduler(opController)
	return &balanceHotRegionsScheduler{
		baseScheduler: base,
		limit:         1,
		stats:         newStoreStaticstics(),
		types:         []BalanceType{hotReadRegionBalance},
		r:             rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func newBalanceHotWriteRegionsScheduler(opController *schedule.OperatorController) *balanceHotRegionsScheduler {
	base := newBaseScheduler(opController)
	return &balanceHotRegionsScheduler{
		baseScheduler: base,
		limit:         1,
		stats:         newStoreStaticstics(),
		types:         []BalanceType{hotWriteRegionBalance},
		r:             rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *balanceHotRegionsScheduler) GetName() string {
	return "balance-hot-region-scheduler"
}

func (h *balanceHotRegionsScheduler) GetType() string {
	return "hot-region"
}

func (h *balanceHotRegionsScheduler) IsScheduleAllowed(cluster schedule.Cluster) bool {
	return h.allowBalanceLeader(cluster) || h.allowBalanceRegion(cluster)
}

func (h *balanceHotRegionsScheduler) allowBalanceLeader(cluster schedule.Cluster) bool {
	return h.opController.OperatorCount(schedule.OpHotRegion) < h.limit &&
		h.opController.OperatorCount(schedule.OpLeader) < cluster.GetLeaderScheduleLimit()
}

func (h *balanceHotRegionsScheduler) allowBalanceRegion(cluster schedule.Cluster) bool {
	return h.opController.OperatorCount(schedule.OpHotRegion) < h.limit &&
		h.opController.OperatorCount(schedule.OpRegion) < cluster.GetRegionScheduleLimit()
}

func (h *balanceHotRegionsScheduler) Schedule(cluster schedule.Cluster) []*schedule.Operator {
	schedulerCounter.WithLabelValues(h.GetName(), "schedule").Inc()
	return h.dispatch(h.types[h.r.Int()%len(h.types)], cluster)
}

func (h *balanceHotRegionsScheduler) dispatch(typ BalanceType, cluster schedule.Cluster) []*schedule.Operator {
	h.Lock()
	defer h.Unlock()
	switch typ {
	case hotReadRegionBalance:
		h.stats.readStatAsLeader = h.calcScore(cluster.RegionReadStats(), cluster, core.LeaderKind)
		return h.balanceHotReadRegions(cluster)
	case hotWriteRegionBalance:
		h.stats.writeStatAsLeader = h.calcScore(cluster.RegionWriteStats(), cluster, core.LeaderKind)
		h.stats.writeStatAsPeer = h.calcScore(cluster.RegionWriteStats(), cluster, core.RegionKind)
		return h.balanceHotWriteRegions(cluster)
	}
	return nil
}

func (h *balanceHotRegionsScheduler) balanceHotReadRegions(cluster schedule.Cluster) []*schedule.Operator {
	// balance by leader
	srcRegion, newLeader := h.balanceByLeader(cluster, h.stats.readStatAsLeader)
	if srcRegion != nil {
		schedulerCounter.WithLabelValues(h.GetName(), "move_leader").Inc()
		// step := schedule.TransferLeader{FromStore: srcRegion.GetLeader().GetStoreId(), ToStore: newLeader.GetStoreId()}
		_ = schedule.TransferLeader{FromStore: srcRegion.GetLeader().GetStoreId(), ToStore: newLeader.GetStoreId()}
		// return []*schedule.Operator{schedule.NewOperator("transferHotReadLeader", srcRegion.GetID(), srcRegion.GetRegionEpoch(), schedule.OpHotRegion|schedule.OpLeader, step)}
		return nil
	}

	// balance by peer
	srcRegion, srcPeer, destPeer := h.balanceByPeer(cluster, h.stats.readStatAsLeader)
	if srcRegion != nil {
		schedulerCounter.WithLabelValues(h.GetName(), "move_peer").Inc()
		return []*schedule.Operator{schedule.CreateMovePeerOperator("moveHotReadRegion", cluster, srcRegion, schedule.OpHotRegion, srcPeer.GetStoreId(), destPeer.GetStoreId(), destPeer.GetId())}
	}
	schedulerCounter.WithLabelValues(h.GetName(), "skip").Inc()
	return nil
}

// balanceHotRetryLimit is the limit to retry schedule for selected balance strategy.
const balanceHotRetryLimit = 10

func (h *balanceHotRegionsScheduler) balanceHotWriteRegions(cluster schedule.Cluster) []*schedule.Operator {
	for i := 0; i < balanceHotRetryLimit; i++ {
		switch h.r.Int() % 2 {
		case 0:
			// balance by peer
			srcRegion, srcPeer, destPeer := h.balanceByPeer(cluster, h.stats.writeStatAsPeer)
			if srcRegion != nil {
				schedulerCounter.WithLabelValues(h.GetName(), "move_peer").Inc()
				fmt.Println(srcRegion, srcPeer, destPeer)
				// return []*schedule.Operator{schedule.CreateMovePeerOperator("moveHotWriteRegion", cluster, srcRegion, schedule.OpHotRegion, srcPeer.GetStoreId(), destPeer.GetStoreId(), destPeer.GetId())}
				return nil
			}
		case 1:
			// balance by leader
			srcRegion, newLeader := h.balanceByLeader(cluster, h.stats.writeStatAsLeader)
			if srcRegion != nil {
				schedulerCounter.WithLabelValues(h.GetName(), "move_leader").Inc()
				// step := schedule.TransferLeader{FromStore: srcRegion.GetLeader().GetStoreId(), ToStore: newLeader.GetStoreId()}
				_ = schedule.TransferLeader{FromStore: srcRegion.GetLeader().GetStoreId(), ToStore: newLeader.GetStoreId()}

				// return []*schedule.Operator{schedule.NewOperator("transferHotWriteLeader", srcRegion.GetID(), srcRegion.GetRegionEpoch(), schedule.OpHotRegion|schedule.OpLeader, step)}
				return nil
			}
		}
	}

	schedulerCounter.WithLabelValues(h.GetName(), "skip").Inc()
	return nil
}

func (h *balanceHotRegionsScheduler) calcScore(items []*core.RegionStat, cluster schedule.Cluster, kind core.ResourceKind) core.StoreHotRegionsStat {
	stats := make(core.StoreHotRegionsStat)
	for _, r := range items {
		if r.HotDegree < cluster.GetHotRegionLowThreshold() {
			continue
		}

		regionInfo := cluster.GetRegion(r.RegionID)
		if regionInfo == nil {
			continue
		}

		var storeIDs []uint64
		switch kind {
		case core.RegionKind:
			for id := range regionInfo.GetStoreIds() {
				storeIDs = append(storeIDs, id)
			}
		case core.LeaderKind:
			storeIDs = append(storeIDs, regionInfo.GetLeader().GetStoreId())
		}

		for _, storeID := range storeIDs {
			storeStat, ok := stats[storeID]
			if !ok {
				storeStat = &core.HotRegionsStat{
					RegionsStat: make(core.RegionsStat, 0, storeHotRegionsDefaultLen),
				}
				stats[storeID] = storeStat
			}

			s := core.RegionStat{
				RegionID:       r.RegionID,
				FlowBytes:      uint64(r.Stats.Median()),
				HotDegree:      r.HotDegree,
				LastUpdateTime: r.LastUpdateTime,
				StoreID:        storeID,
				AntiCount:      r.AntiCount,
				Version:        r.Version,
			}
			storeStat.TotalFlowBytes += r.FlowBytes
			storeStat.RegionsCount++
			storeStat.RegionsStat = append(storeStat.RegionsStat, s)
		}
	}
	return stats
}

func (h *balanceHotRegionsScheduler) balanceByPeer(cluster schedule.Cluster, storesStat core.StoreHotRegionsStat) (*core.RegionInfo, *metapb.Peer, *metapb.Peer) {
	if !h.allowBalanceRegion(cluster) {
		return nil, nil, nil
	}

	srcStoreID := h.selectSrcStore(storesStat)
	if srcStoreID == 0 {
		return nil, nil, nil
	}

	// get one source region and a target store.
	// For each region in the source store, we try to find the best target store;
	// If we can find a target store, then return from this method.
	stores := cluster.GetStores()
	var destStoreID uint64
	for _, i := range h.r.Perm(storesStat[srcStoreID].RegionsStat.Len()) {
		rs := storesStat[srcStoreID].RegionsStat[i]
		srcRegion := cluster.GetRegion(rs.RegionID)
		if srcRegion == nil || len(srcRegion.GetDownPeers()) != 0 || len(srcRegion.GetPendingPeers()) != 0 {
			continue
		}

		srcStore := cluster.GetStore(srcStoreID)
		filters := []schedule.Filter{
			schedule.StoreStateFilter{MoveRegion: true},
			schedule.NewExcludedFilter(srcRegion.GetStoreIds(), srcRegion.GetStoreIds()),
			schedule.NewDistinctScoreFilter(cluster.GetLocationLabels(), cluster.GetRegionStores(srcRegion), srcStore),
		}
		destStoreIDs := make([]uint64, 0, len(stores))
		for _, store := range stores {
			if schedule.FilterTarget(cluster, store, filters) {
				continue
			}
			destStoreIDs = append(destStoreIDs, store.GetId())
		}

		destStoreID, _ = h.selectDestStore(destStoreIDs, rs.FlowBytes, srcStoreID, storesStat)
		if destStoreID != 0 {
			h.adjustBalanceLimit(srcStoreID, storesStat)

			srcPeer := srcRegion.GetStorePeer(srcStoreID)
			if srcPeer == nil {
				return nil, nil, nil
			}

			// When the target store is decided, we allocate a peer ID to hold the source region,
			// because it doesn't exist in the system right now.
			destPeer, err := cluster.AllocPeer(destStoreID)
			if err != nil {
				log.Errorf("failed to allocate peer: %v", err)
				return nil, nil, nil
			}

			return srcRegion, srcPeer, destPeer
		}
	}

	return nil, nil, nil
}

func (h *balanceHotRegionsScheduler) balanceByLeader(cluster schedule.Cluster, storesStat core.StoreHotRegionsStat) (*core.RegionInfo, *metapb.Peer) {
	if !h.allowBalanceLeader(cluster) {
		return nil, nil
	}

	srcStoreID := h.selectSrcStore(storesStat)
	if srcStoreID == 0 {
		return nil, nil
	}

	// select destPeer
	for _, i := range h.r.Perm(storesStat[srcStoreID].RegionsStat.Len()) {
		rs := storesStat[srcStoreID].RegionsStat[i]
		srcRegion := cluster.GetRegion(rs.RegionID)
		if srcRegion == nil || len(srcRegion.GetDownPeers()) != 0 || len(srcRegion.GetPendingPeers()) != 0 {
			continue
		}

		filters := []schedule.Filter{schedule.StoreStateFilter{TransferLeader: true}}
		candidateStoreIDs := make([]uint64, 0, len(srcRegion.GetPeers())-1)
		for _, store := range cluster.GetFollowerStores(srcRegion) {
			if !schedule.FilterTarget(cluster, store, filters) {
				candidateStoreIDs = append(candidateStoreIDs, store.GetId())
			}
		}
		if len(candidateStoreIDs) == 0 {
			continue
		}
		destStoreID, mstr := h.selectDestStore(candidateStoreIDs, rs.FlowBytes, srcStoreID, storesStat)
		if destStoreID == 0 {
			postJSON("", mstr, srcStoreID, destStoreID)
			continue
		}

		destPeer := srcRegion.GetStoreVoter(destStoreID)
		if destPeer != nil {
			h.adjustBalanceLimit(srcStoreID, storesStat)
			step := schedule.TransferLeader{FromStore: srcRegion.GetLeader().GetStoreId(), ToStore: destPeer.GetStoreId()}
			postJSON(step.String(), mstr, srcStoreID, destStoreID)
			return srcRegion, destPeer
		}
	}
	return nil, nil
}

func postJSON(s string, ms []Feature, srcStoreID, destStoreID uint64) {
	if s == "" || ms == nil {
		log.Println("[HOT] step is empty, ms is nil ")
		return
	}
	b, err := json.Marshal(ms)
	if err != nil {
		log.Println(err)
	}

	step := "[" + "\"" + s + "\"" + ","
	str := "{\"updates\":[" + step + string(b) + "],"

	str = str[:len(str)-1]
	str = str + "]}"

	// PUT model service
	httpClient("PUT", str, srcStoreID, destStoreID)

	// POST model
	gstr := "{\"features\": [" + string(b) + "]}"
	httpClient("POST", gstr, srcStoreID, destStoreID)
}

var reqURL = "http://localhost:8000/model/pd"

func httpClient(method, jsonStr string, srcStoreID, destStoreID uint64) {
	logStr := "[HOT] method:" + method + ", URL:>" + reqURL

	req, err := http.NewRequest(method, reqURL, strings.NewReader(jsonStr))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)

	if resp == nil || err != nil {
		log.Println(logStr+", http request error or resp is nil, ", err)
		return
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	headStr := fmt.Sprintf("%v", resp.Header)
	logStr += ", response Status:" + resp.Status + ", response Headers:"
	+headStr + ", response Body:" + string(body)
	if strings.Contains(string(body), "predictions") {
		var maxProbability float64
		var v map[string][]interface{}
		json.Unmarshal(body, &v)
		v2 := v["predictions"]
		var ke string
		for k, v := range v2[0].(map[string]interface{}) {
			if maxProbability < v.(float64) {
				maxProbability = v.(float64)
				ke = k
			}
		}
		logStr += "\nsuggest step: " + ke + ", maxProbability:" + fmt.Sprintf("%.15f", maxProbability)
		srcStoreIDD, _ := strconv.Atoi(ke[27:28])
		destStoreIDD, _ := strconv.Atoi(ke[38:39])
		if srcStoreID == uint64(srcStoreIDD) && destStoreID == uint64(destStoreIDD) {
			logStr += " - [HIT]"
		} else {
			logStr += " - [MISS], srcStoreID:" + strconv.Itoa(int(srcStoreID)) + ", destStoreID:" + strconv.Itoa(int(destStoreID))
		}
	}
	log.Println(logStr)
}

// Select the store to move hot regions from.
// We choose the store with the maximum number of hot region first.
// Inside these stores, we choose the one with maximum flow bytes.
func (h *balanceHotRegionsScheduler) selectSrcStore(stats core.StoreHotRegionsStat) (srcStoreID uint64) {
	var (
		maxFlowBytes           uint64
		maxHotStoreRegionCount int
	)

	for storeID, statistics := range stats {
		count, flowBytes := statistics.RegionsStat.Len(), statistics.TotalFlowBytes
		if count >= 2 && (count > maxHotStoreRegionCount || (count == maxHotStoreRegionCount && flowBytes > maxFlowBytes)) {
			maxHotStoreRegionCount = count
			maxFlowBytes = flowBytes
			srcStoreID = storeID
		}
	}
	return
}

type Feature struct {
	// 	[{"feature_type":"Category", "name":"hotRegionsCount1", "value":"true"},{"feature_type":"Category", "name":"minRegionsCount1", "value":"true"}]
	FeatureType string `json:"feature_type"`
	Name        string `json:"name"`
	Value       string `json:"value"`
}

// selectDestStore selects a target store to hold the region of the source region.
// We choose a target store based on the hot region number and flow bytes of this store.
func (h *balanceHotRegionsScheduler) selectDestStore(candidateStoreIDs []uint64, regionFlowBytes uint64, srcStoreID uint64, storesStat core.StoreHotRegionsStat) (uint64, []Feature) {
	sr := storesStat[srcStoreID]
	srcFlowBytes := sr.TotalFlowBytes
	srcHotRegionsCount := sr.RegionsStat.Len()

	var (
		destStoreID     uint64
		minFlowBytes    uint64 = math.MaxUint64
		minRegionsCount        = int(math.MaxInt32)
	)
	var strategies []Feature
	for _, storeID := range candidateStoreIDs {
		if s, ok := storesStat[storeID]; ok {
			if srcHotRegionsCount-s.RegionsStat.Len() > 1 && minRegionsCount > s.RegionsStat.Len() {
				destStoreID = storeID
				minFlowBytes = s.TotalFlowBytes
				minRegionsCount = s.RegionsStat.Len()
				str1 := fmt.Sprintf("hotRegionsCount%d", storeID)
				str2 := fmt.Sprintf("minRegionsCount%d", storeID)
				strategy := Feature{}
				strategy.FeatureType = "Category"
				strategy.Name = str1
				strategy.Value = "true"
				strategies = append(strategies, strategy)
				strategy1 := Feature{}
				strategy1.FeatureType = "Category"
				strategy1.Name = str2
				strategy1.Value = "true"
				strategies = append(strategies, strategy1)
				continue
			}
			if minRegionsCount == s.RegionsStat.Len() && minFlowBytes > s.TotalFlowBytes &&
				uint64(float64(srcFlowBytes)*hotRegionScheduleFactor) > s.TotalFlowBytes+2*regionFlowBytes {
				minFlowBytes = s.TotalFlowBytes
				destStoreID = storeID
				str1 := fmt.Sprintf("minFlowBytes%d", storeID)
				str2 := fmt.Sprintf("srcFlowBytes%d", storeID)
				strategy2 := Feature{}
				strategy2.FeatureType = "Category"
				strategy2.Name = str1
				strategy2.Value = "true"
				strategies = append(strategies, strategy2)
				strategy3 := Feature{}
				strategy3.FeatureType = "Category"
				strategy3.Name = str2
				strategy3.Value = "true"
				strategies = append(strategies, strategy3)
			}
		} else {
			destStoreID = storeID
			return destStoreID, strategies
		}
	}
	strategy := Feature{}
	strategy.FeatureType = "Category"
	strategy.Name = "srcRegion"
	strategy.Value = fmt.Sprintf("%d", srcStoreID)
	strategies = append(strategies, strategy)
	return destStoreID, strategies
}

func (h *balanceHotRegionsScheduler) adjustBalanceLimit(storeID uint64, storesStat core.StoreHotRegionsStat) {
	srcStoreStatistics := storesStat[storeID]

	var hotRegionTotalCount float64
	for _, m := range storesStat {
		hotRegionTotalCount += float64(m.RegionsStat.Len())
	}

	avgRegionCount := hotRegionTotalCount / float64(len(storesStat))
	// Multiplied by hotRegionLimitFactor to avoid transfer back and forth
	limit := uint64((float64(srcStoreStatistics.RegionsStat.Len()) - avgRegionCount) * hotRegionLimitFactor)
	h.limit = maxUint64(1, limit)
}

func (h *balanceHotRegionsScheduler) GetHotReadStatus() *core.StoreHotRegionInfos {
	h.RLock()
	defer h.RUnlock()
	asLeader := make(core.StoreHotRegionsStat, len(h.stats.readStatAsLeader))
	for id, stat := range h.stats.readStatAsLeader {
		clone := *stat
		asLeader[id] = &clone
	}
	return &core.StoreHotRegionInfos{
		AsLeader: asLeader,
	}
}

func (h *balanceHotRegionsScheduler) GetHotWriteStatus() *core.StoreHotRegionInfos {
	h.RLock()
	defer h.RUnlock()
	asLeader := make(core.StoreHotRegionsStat, len(h.stats.writeStatAsLeader))
	asPeer := make(core.StoreHotRegionsStat, len(h.stats.writeStatAsPeer))
	for id, stat := range h.stats.writeStatAsLeader {
		clone := *stat
		asLeader[id] = &clone
	}
	for id, stat := range h.stats.writeStatAsPeer {
		clone := *stat
		asPeer[id] = &clone
	}
	return &core.StoreHotRegionInfos{
		AsLeader: asLeader,
		AsPeer:   asPeer,
	}
}
