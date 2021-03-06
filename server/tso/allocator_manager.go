// Copyright 2020 TiKV Project Authors.
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

package tso

import (
	"context"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/slice"
	"github.com/tikv/pd/server/config"
	"github.com/tikv/pd/server/election"
	"github.com/tikv/pd/server/member"
	"go.uber.org/zap"
)

const (
	leaderTickInterval          = 50 * time.Millisecond
	defaultAllocatorLeaderLease = 3
)

// AllocatorGroupFilter is used to select AllocatorGroup.
type AllocatorGroupFilter func(ag *allocatorGroup) bool

type allocatorGroup struct {
	dcLocation string
	// allocator's parent ctx and cancel function, which is to
	// control the allocator's behavior in AllocatorDaemon and
	// pass the cancel signal to its parent as soon as possible
	// since it is critical to let parent goroutine know whether
	// the allocator is still able to work well.
	parentCtx    context.Context
	parentCancel context.CancelFunc
	// For the Global TSO Allocator, leadership is a PD leader's
	// leadership, and for the Local TSO Allocator, leadership
	// is a DC-level certificate to allow an allocator to generate
	// TSO for local transactions in its DC.
	leadership *election.Leadership
	allocator  Allocator
}

// AllocatorManager is used to manage the TSO Allocators a PD server holds.
// It is in charge of maintaining TSO allocators' leadership, checking election
// priority, and forwarding TSO allocation requests to correct TSO Allocators.
type AllocatorManager struct {
	sync.RWMutex
	wg sync.WaitGroup
	// There are two kinds of TSO Allocators:
	//   1. Global TSO Allocator, as a global single point to allocate
	//      TSO for global transactions, such as cross-region cases.
	//   2. Local TSO Allocator, servers for DC-level transactions.
	// dc-location/global (string) -> TSO Allocator
	allocatorGroups map[string]*allocatorGroup
	// for election use
	member *member.Member
	// tso config
	rootPath      string
	saveInterval  time.Duration
	maxResetTSGap func() time.Duration
}

// NewAllocatorManager creates a new TSO Allocator Manager.
func NewAllocatorManager(m *member.Member, rootPath string, saveInterval time.Duration, maxResetTSGap func() time.Duration) *AllocatorManager {
	allocatorManager := &AllocatorManager{
		allocatorGroups: make(map[string]*allocatorGroup),
		member:          m,
		rootPath:        rootPath,
		saveInterval:    saveInterval,
		maxResetTSGap:   maxResetTSGap,
	}
	return allocatorManager
}

// SetUpAllocator is used to set up an allocator, which will initialize the allocator and put it into allocator daemon.
func (am *AllocatorManager) SetUpAllocator(parentCtx context.Context, parentCancel context.CancelFunc, dcLocation string, leadership *election.Leadership) error {
	var allocator Allocator
	if dcLocation == config.GlobalDCLocation {
		allocator = NewGlobalTSOAllocator(leadership, am.getAllocatorPath(dcLocation), am.saveInterval, am.maxResetTSGap)
	} else {
		allocator = NewLocalTSOAllocator(am.member, leadership, am.getAllocatorPath(dcLocation), dcLocation, am.saveInterval, am.maxResetTSGap)
	}
	am.Lock()
	defer am.Unlock()
	// Update or create a new allocatorGroup
	am.allocatorGroups[dcLocation] = &allocatorGroup{
		dcLocation:   dcLocation,
		parentCtx:    parentCtx,
		parentCancel: parentCancel,
		leadership:   leadership,
		allocator:    allocator,
	}
	// Different kinds of allocators have different setup works to do
	switch dcLocation {
	// For Global TSO Allocator
	case config.GlobalDCLocation:
		// Because Global TSO Allocator only depends on PD leader's leadership,
		// so we can directly initialize it here.
		if err := am.allocatorGroups[dcLocation].allocator.Initialize(); err != nil {
			return err
		}
	// For Local TSO Allocator
	default:
		// Join in a Local TSO Allocator election
		localTSOAllocator, _ := allocator.(*LocalTSOAllocator)
		go am.allocatorLeaderLoop(parentCtx, localTSOAllocator)
	}
	return nil
}

func (am *AllocatorManager) getAllocatorPath(dcLocation string) string {
	// For backward compatibility, the global timestamp's store path will still use the old one
	if dcLocation == config.GlobalDCLocation {
		return am.rootPath
	}
	return path.Join(am.rootPath, dcLocation)
}

// similar logic with leaderLoop in server/server.go
func (am *AllocatorManager) allocatorLeaderLoop(parentCtx context.Context, allocator *LocalTSOAllocator) {
	for {
		select {
		case <-parentCtx.Done():
			log.Info("server is closed, return local tso allocator leader loop",
				zap.String("dc-location", allocator.dcLocation),
				zap.String("local-tso-allocator-name", am.member.Member().Name))
			return
		default:
		}

		allocatorLeader, rev, checkAgain := allocator.CheckAllocatorLeader()
		if checkAgain {
			continue
		}
		if allocatorLeader != nil {
			log.Info("start to watch allocator leader",
				zap.Stringer(fmt.Sprintf("%s-allocator-leader", allocator.dcLocation), allocatorLeader),
				zap.String("local-tso-allocator-name", am.member.Member().Name))
			// WatchAllocatorLeader will keep looping and never return unless the Local TSO Allocator leader has changed.
			allocator.WatchAllocatorLeader(parentCtx, allocatorLeader, rev)
			log.Info("local tso allocator leader has changed, try to re-campaign a local tso allocator leader",
				zap.String("dc-location", allocator.dcLocation))
		}
		am.campaignAllocatorLeader(parentCtx, allocator)
	}
}

func (am *AllocatorManager) campaignAllocatorLeader(parentCtx context.Context, allocator *LocalTSOAllocator) {
	log.Info("start to campaign local tso allocator leader",
		zap.String("dc-location", allocator.dcLocation),
		zap.String("name", am.member.Member().Name))
	if err := allocator.CampaignAllocatorLeader(defaultAllocatorLeaderLease); err != nil {
		log.Error("failed to campaign local tso allocator leader", errs.ZapError(err))
		return
	}

	// Start keepalive the Local TSO Allocator leadership and enable Local TSO service.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	defer am.resetAllocatorGroup(allocator.dcLocation)
	// maintain the Local TSO Allocator leader
	go allocator.KeepAllocatorLeader(ctx)
	log.Info("campaign local tso allocator leader ok",
		zap.String("dc-location", allocator.dcLocation),
		zap.String("name", am.member.Member().Name))

	log.Info("initialize the local TSO allocator",
		zap.String("dc-location", allocator.dcLocation),
		zap.String("name", am.member.Member().Name))
	if err := allocator.Initialize(); err != nil {
		log.Error("failed to initialize the local TSO allocator", errs.ZapError(err))
		return
	}
	allocator.EnableAllocatorLeader()
	log.Info("local tso allocator leader is ready to serve",
		zap.String("dc-location", allocator.dcLocation),
		zap.String("name", am.member.Member().Name))

	leaderTicker := time.NewTicker(leaderTickInterval)
	defer leaderTicker.Stop()

	for {
		select {
		case <-leaderTicker.C:
			if !allocator.IsStillAllocatorLeader() {
				log.Info("no longer a local tso allocator leader because lease has expired, local tso allocator leader will step down",
					zap.String("dc-location", allocator.dcLocation),
					zap.String("name", am.member.Member().Name))
				return
			}
		case <-ctx.Done():
			// Server is closed and it should return nil.
			log.Info("server is closed, reset the local tso allocator",
				zap.String("dc-location", allocator.dcLocation),
				zap.String("name", am.member.Member().Name))
			return
		}
	}
}

// AllocatorDaemon is used to update every allocator's TSO.
func (am *AllocatorManager) AllocatorDaemon(serverCtx context.Context) {
	tsTicker := time.NewTicker(UpdateTimestampStep)
	defer tsTicker.Stop()

	for {
		select {
		case <-tsTicker.C:
			// Filter out allocators without leadership and uninitialized
			allocatorGroups := am.getAllocatorGroups(FilterUninitialized(), FilterUnavailableLeadership())
			// Update each allocator concurrently
			for _, ag := range allocatorGroups {
				am.wg.Add(1)
				go am.updateAllocator(ag)
			}
			am.wg.Wait()
		case <-serverCtx.Done():
			return
		}
	}
}

// updateAllocator is used to update the allocator in the group.
func (am *AllocatorManager) updateAllocator(ag *allocatorGroup) {
	defer am.wg.Done()
	select {
	case <-ag.parentCtx.Done():
		// Resetting the allocator will clear TSO in memory
		ag.allocator.Reset()
		return
	default:
	}
	if !ag.leadership.Check() {
		log.Info("allocator doesn't campaign leadership yet", zap.String("dc-location", ag.dcLocation))
		time.Sleep(200 * time.Millisecond)
		return
	}
	if err := ag.allocator.UpdateTSO(); err != nil {
		log.Warn("failed to update allocator's timestamp", zap.String("dc-location", ag.dcLocation), errs.ZapError(err))
		ag.parentCancel()
		return
	}
}

// HandleTSORequest forwards TSO allocation requests to correct TSO Allocators.
func (am *AllocatorManager) HandleTSORequest(dcLocation string, count uint32) (pdpb.Timestamp, error) {
	am.RLock()
	defer am.RUnlock()
	allocatorGroup, exist := am.allocatorGroups[dcLocation]
	if !exist {
		err := errs.ErrGetAllocator.FastGenByArgs(fmt.Sprintf("%s allocator not found, generate timestamp failed", dcLocation))
		return pdpb.Timestamp{}, err
	}
	return allocatorGroup.allocator.GenerateTSO(count)
}

func (am *AllocatorManager) resetAllocatorGroup(dcLocation string) {
	am.Lock()
	defer am.Unlock()
	if allocatorGroup, exist := am.allocatorGroups[dcLocation]; exist {
		allocatorGroup.allocator.Reset()
		allocatorGroup.leadership.Reset()
	}
}

func (am *AllocatorManager) getAllocatorGroups(filters ...AllocatorGroupFilter) []*allocatorGroup {
	am.RLock()
	defer am.RUnlock()
	allocatorGroups := make([]*allocatorGroup, 0)
	for _, ag := range am.allocatorGroups {
		if ag == nil {
			continue
		}
		if slice.NoneOf(filters, func(i int) bool { return filters[i](ag) }) {
			allocatorGroups = append(allocatorGroups, ag)
		}
	}
	return allocatorGroups
}

// GetAllocator get the allocator by dc-location.
func (am *AllocatorManager) GetAllocator(dcLocation string) (Allocator, error) {
	am.RLock()
	defer am.RUnlock()
	allocatorGroup, exist := am.allocatorGroups[dcLocation]
	if !exist {
		return nil, errs.ErrGetAllocator.FastGenByArgs(fmt.Sprintf("%s allocator not found", dcLocation))
	}
	return allocatorGroup.allocator, nil
}

// GetAllocators get all allocators with some filters.
func (am *AllocatorManager) GetAllocators(filters ...AllocatorGroupFilter) []Allocator {
	var allocators []Allocator
	for _, ag := range am.getAllocatorGroups(filters...) {
		allocators = append(allocators, ag.allocator)
	}
	return allocators
}
