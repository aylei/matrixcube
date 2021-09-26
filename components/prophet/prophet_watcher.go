// Copyright 2020 MatrixOrigin.
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

package prophet

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/fagongzi/goetty"
	"github.com/matrixorigin/matrixcube/components/prophet/cluster"
	"github.com/matrixorigin/matrixcube/components/prophet/event"
	"github.com/matrixorigin/matrixcube/components/prophet/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/components/prophet/util"
)

type watcherSession struct {
	seq     uint64
	flag    uint32
	session goetty.IOSession
}

func (wt *watcherSession) notify(evt rpcpb.EventNotify) error {
	if event.MatchEvent(evt.Type, wt.flag) {
		resp := &rpcpb.Response{}
		resp.Type = rpcpb.TypeEventNotify
		resp.Event = evt
		resp.Event.Seq = atomic.AddUint64(&wt.seq, 1)
		util.GetLogger().Debugf("write notify event %+v", resp)
		return wt.session.WriteAndFlush(resp)
	}
	return nil
}

type watcherNotifier struct {
	sync.Mutex

	watchers map[uint64]*watcherSession
	cluster  *cluster.RaftCluster
}

func newWatcherNotifier(cluster *cluster.RaftCluster) *watcherNotifier {
	return &watcherNotifier{
		cluster:  cluster,
		watchers: make(map[uint64]*watcherSession),
	}
}

func (wn *watcherNotifier) handleCreateWatcher(req *rpcpb.Request, resp *rpcpb.Response, session goetty.IOSession) error {
	util.GetLogger().Infof("new watcher %s added",
		session.RemoteAddr())

	if wn != nil {
		wn.cluster.RLock()
		defer wn.cluster.RUnlock()
		if event.MatchEvent(event.EventInit, req.CreateWatcher.Flag) {
			snap := event.Snapshot{
				Leaders: make(map[uint64]uint64),
			}
			for _, c := range wn.cluster.GetContainers() {
				snap.Containers = append(snap.Containers, c.Meta.Clone())
			}
			for _, res := range wn.cluster.GetResources() {
				snap.Resources = append(snap.Resources, res.Meta.Clone())
				leader := res.GetLeader()
				if leader != nil {
					snap.Leaders[res.Meta.ID()] = leader.ID
				}
			}

			rsp, err := event.NewInitEvent(snap)
			if err != nil {
				return err
			}

			resp.Event.Type = event.EventInit
			resp.Event.InitEvent = rsp
		}

		return wn.addWatcher(req.CreateWatcher.Flag, session)
	}

	return nil
}

func (wn *watcherNotifier) addWatcher(flag uint32, session goetty.IOSession) error {
	wn.Lock()
	defer wn.Unlock()

	if wn.watchers == nil {
		return fmt.Errorf("watcher notifier stopped")
	}

	wn.watchers[session.ID()] = &watcherSession{
		flag:    flag,
		session: session,
	}
	return nil
}

func (wn *watcherNotifier) doClearWatcherLocked(w *watcherSession) {
	delete(wn.watchers, w.session.ID())
	w.session.Close()
	util.GetLogger().Infof("watcher %s removed",
		w.session.RemoteAddr())
}

func (wn *watcherNotifier) doNotify(evt rpcpb.EventNotify) {
	wn.Lock()
	defer wn.Unlock()

	for _, wt := range wn.watchers {
		err := wt.notify(evt)
		if err != nil {
			wn.doClearWatcherLocked(wt)
		}
	}
}

func (wn *watcherNotifier) start() {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				util.GetLogger().Errorf("watcher notify failed with %+v, restart later", err)
				wn.start()
			}
		}()

		for {
			evt, ok := <-wn.cluster.ChangedEventNotifier()
			if !ok {
				util.GetLogger().Infof("watcher notifer exited")
				return
			}

			wn.doNotify(evt)
		}
	}()
}

func (wn *watcherNotifier) stop() {
	wn.Lock()
	defer wn.Unlock()

	for _, wt := range wn.watchers {
		wn.doClearWatcherLocked(wt)
	}
	wn.watchers = nil
	util.GetLogger().Infof("watcher notifier stopped")
}
