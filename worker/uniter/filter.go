// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package uniter

import (
	"fmt"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/errors"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/watcher"
	"launchpad.net/juju-core/worker"
	"launchpad.net/tomb"
	"sort"
)

// filter collects unit, service, and service config information from separate
// state watchers, and presents it as events on channels designed specifically
// for the convenience of the uniter.
type filter struct {
	st   *state.State
	tomb tomb.Tomb

	// outUnitDying is closed when the unit's life becomes Dying.
	outUnitDying chan struct{}

	// The out*On chans are used to deliver events to clients.
	// The out* chans, when set to the corresponding out*On chan (rather than
	// nil) indicate that an event of the appropriate type is ready to send
	// to the client.
	outConfig      chan struct{}
	outConfigOn    chan struct{}
	outUpgrade     chan *charm.URL
	outUpgradeOn   chan *charm.URL
	outResolved    chan state.ResolvedMode
	outResolvedOn  chan state.ResolvedMode
	outRelations   chan []int
	outRelationsOn chan []int

	// The want* chans are used to indicate that the filter should send
	// events if it has them available.
	wantForcedUpgrade chan bool
	wantResolved      chan struct{}

	// discardConfig is used to indicate that any pending config event
	// should be discarded.
	discardConfig chan struct{}

	// setCharm is used to request that the unit's charm URL be set to
	// a new value. This must be done in the filter's goroutine, so
	// that config watches can be stopped and restarted pointing to
	// the new charm URL. If we don't stop the watch before the
	// (potentially) last reference to that settings document is
	// removed, we'll see spurious errors (and even in the best case,
	// we risk getting notifications for the wrong settings version).
	setCharm chan *charm.URL

	// didSetCharm is used to report back after setting a charm URL.
	didSetCharm chan struct{}

	// clearResolved is used to request that the unit's resolved flag
	// be cleared. This must be done on the filter's goroutine so that
	// it can immediately trigger the unit change handler, and thus
	// ensure that subsquent requests for resolved events -- that land
	// before the next watcher update for the unit -- do not erroneously
	// send out stale values.
	clearResolved chan struct{}

	// didClearResolved is used to report back after clearing the resolved
	// flag.
	didClearResolved chan struct{}

	// The following fields hold state that is collected while running,
	// and used to detect interesting changes to express as events.
	unit             *state.Unit
	life             state.Life
	resolved         state.ResolvedMode
	service          *state.Service
	upgradeFrom      serviceCharm
	upgradeAvailable serviceCharm
	upgrade          *charm.URL
	relations        []int
}

// newFilter returns a filter that handles state changes pertaining to the
// supplied unit.
func newFilter(st *state.State, unitName string) (*filter, error) {
	f := &filter{
		st:                st,
		outUnitDying:      make(chan struct{}),
		outConfig:         make(chan struct{}),
		outConfigOn:       make(chan struct{}),
		outUpgrade:        make(chan *charm.URL),
		outUpgradeOn:      make(chan *charm.URL),
		outResolved:       make(chan state.ResolvedMode),
		outResolvedOn:     make(chan state.ResolvedMode),
		outRelations:      make(chan []int),
		outRelationsOn:    make(chan []int),
		wantForcedUpgrade: make(chan bool),
		wantResolved:      make(chan struct{}),
		discardConfig:     make(chan struct{}),
		setCharm:          make(chan *charm.URL),
		didSetCharm:       make(chan struct{}),
		clearResolved:     make(chan struct{}),
		didClearResolved:  make(chan struct{}),
	}
	go func() {
		defer f.tomb.Done()
		err := f.loop(unitName)
		log.Errorf("worker/uniter/filter: %v", err)
		f.tomb.Kill(err)
	}()
	return f, nil
}

func (f *filter) Stop() error {
	f.tomb.Kill(nil)
	return f.tomb.Wait()
}

func (f *filter) Dead() <-chan struct{} {
	return f.tomb.Dead()
}

func (f *filter) Wait() error {
	return f.tomb.Wait()
}

// UnitDying returns a channel which is closed when the Unit enters a Dying state.
func (f *filter) UnitDying() <-chan struct{} {
	return f.outUnitDying
}

// UpgradeEvents returns a channel that will receive a new charm URL whenever an
// upgrade is indicated. Events should not be read until the baseline state
// has been specified by calling WantUpgradeEvent.
func (f *filter) UpgradeEvents() <-chan *charm.URL {
	return f.outUpgradeOn
}

// ResolvedEvents returns a channel that may receive a ResolvedMode when the
// unit's Resolved value changes, or when an event is explicitly requested.
// A ResolvedNone state will never generate events, but ResolvedRetryHooks and
// ResolvedNoHooks will always be delivered as described.
func (f *filter) ResolvedEvents() <-chan state.ResolvedMode {
	return f.outResolvedOn
}

// ConfigEvents returns a channel that will receive a signal whenever the service's
// configuration changes, or when an event is explicitly requested.
func (f *filter) ConfigEvents() <-chan struct{} {
	return f.outConfigOn
}

// RelationsEvents returns a channel that will receive the ids of all the service's
// relations whose Life status has changed.
func (f *filter) RelationsEvents() <-chan []int {
	return f.outRelationsOn
}

// WantUpgradeEvent controls whether the filter will generate upgrade
// events for unforced service charm changes.
func (f *filter) WantUpgradeEvent(mustForce bool) {
	select {
	case <-f.tomb.Dying():
	case f.wantForcedUpgrade <- mustForce:
	}
}

// SetCharm notifies the filter that the unit is running a new
// charm. It causes the unit's charm URL to be set in state, and the
// following changes to the filter's behaviour:
//
// * Upgrade events will only be generated for charms different to
//   that supplied;
// * A fresh relations event will be generated containing every relation
//   the service is participating in;
// * A fresh configuration event will be generated, and subsequent
//   events will only be sent in response to changes in the version
//   of the service's settings that is specific to that charm.
//
// SetCharm blocks until the charm URL is set in state, returning any
// error that occurred.
func (f *filter) SetCharm(curl *charm.URL) error {
	select {
	case <-f.tomb.Dying():
		return tomb.ErrDying
	case f.setCharm <- curl:
	}
	select {
	case <-f.tomb.Dying():
		return tomb.ErrDying
	case <-f.didSetCharm:
		return nil
	}
	panic("unreachable")
}

// WantResolvedEvent indicates that the filter should send a resolved event
// if one is available.
func (f *filter) WantResolvedEvent() {
	select {
	case <-f.tomb.Dying():
	case f.wantResolved <- nothing:
	}
}

// ClearResolved notifies the filter that a resolved event has been handled
// and should not be reported again.
func (f *filter) ClearResolved() error {
	select {
	case <-f.tomb.Dying():
		return tomb.ErrDying
	case f.clearResolved <- nothing:
	}
	select {
	case <-f.tomb.Dying():
		return tomb.ErrDying
	case <-f.didClearResolved:
		log.Debugf("resolved clear completed")
		return nil
	}
	panic("unreachable")
}

// DiscardConfigEvent indicates that the filter should discard any pending
// config event.
func (f *filter) DiscardConfigEvent() {
	select {
	case <-f.tomb.Dying():
	case f.discardConfig <- nothing:
	}
}

func (f *filter) loop(unitName string) (err error) {
	f.unit, err = f.st.Unit(unitName)
	if err != nil {
		return err
	}
	if err = f.unitChanged(); err != nil {
		return err
	}
	f.service, err = f.unit.Service()
	if err != nil {
		return err
	}
	if err = f.serviceChanged(); err != nil {
		return err
	}
	unitw := f.unit.Watch()
	defer watcher.Stop(unitw, &f.tomb)
	servicew := f.service.Watch()
	defer watcher.Stop(servicew, &f.tomb)
	// configw and relationsw can get restarted, so we need to use
	// their eventual values in the defer calls.
	var configw state.NotifyWatcher
	var configChanges <-chan struct{}
	if curl, ok := f.unit.CharmURL(); ok {
		configw, err = f.unit.WatchConfigSettings()
		if err != nil {
			return err
		}
		configChanges = configw.Changes()
		f.upgradeFrom.url = curl
	}
	defer func() {
		if configw != nil {
			watcher.Stop(configw, &f.tomb)
		}
	}()
	relationsw := f.service.WatchRelations()
	defer func() { watcher.Stop(relationsw, &f.tomb) }()

	// Config events cannot be meaningfully discarded until one is available;
	// once we receive the initial change, we unblock discard requests by
	// setting this channel to its namesake on f.
	var discardConfig chan struct{}
	for {
		var ok bool
		select {
		case <-f.tomb.Dying():
			return tomb.ErrDying

		// Handle watcher changes.
		case _, ok = <-unitw.Changes():
			log.Debugf("worker/uniter/filter: got unit change")
			if !ok {
				return watcher.MustErr(unitw)
			}
			if err = f.unitChanged(); err != nil {
				return err
			}
		case _, ok = <-servicew.Changes():
			log.Debugf("worker/uniter/filter: got service change")
			if !ok {
				return watcher.MustErr(servicew)
			}
			if err = f.serviceChanged(); err != nil {
				return err
			}
		case _, ok = <-configChanges:
			log.Debugf("worker/uniter/filter: got config change")
			if !ok {
				return watcher.MustErr(configw)
			}
			log.Debugf("worker/uniter/filter: preparing new config event")
			f.outConfig = f.outConfigOn
			discardConfig = f.discardConfig
		case ids, ok := <-relationsw.Changes():
			log.Debugf("worker/uniter/filter: got relations change")
			if !ok {
				return watcher.MustErr(relationsw)
			}
			f.relationsChanged(ids)

		// Send events on active out chans.
		case f.outUpgrade <- f.upgrade:
			log.Debugf("worker/uniter/filter: sent upgrade event")
			f.outUpgrade = nil
		case f.outResolved <- f.resolved:
			log.Debugf("worker/uniter/filter: sent resolved event")
			f.outResolved = nil
		case f.outConfig <- nothing:
			log.Debugf("worker/uniter/filter: sent config event")
			f.outConfig = nil
		case f.outRelations <- f.relations:
			log.Debugf("worker/uniter/filter: sent relations event")
			f.outRelations = nil
			f.relations = nil

		// Handle explicit requests.
		case curl := <-f.setCharm:
			log.Debugf("worker/uniter/filter: changing charm to %q", curl)
			// We need to restart the config watcher after setting the
			// charm, because service config settings are distinct for
			// different service charms.
			if configw != nil {
				if err := configw.Stop(); err != nil {
					return err
				}
			}
			if err := f.unit.SetCharmURL(curl); err != nil {
				log.Debugf("worker/uniter/filter: failed setting charm url %q: %v", curl, err)
				return err
			}
			select {
			case <-f.tomb.Dying():
				return tomb.ErrDying
			case f.didSetCharm <- nothing:
			}
			configw, err = f.unit.WatchConfigSettings()
			if err != nil {
				return err
			}
			configChanges = configw.Changes()

			// Restart the relations watcher.
			if err := relationsw.Stop(); err != nil {
				return err
			}
			relationsw = f.service.WatchRelations()

			f.upgradeFrom.url = curl
			if err = f.upgradeChanged(); err != nil {
				return err
			}
		case force := <-f.wantForcedUpgrade:
			log.Debugf("worker/uniter/filter: want forced upgrade %v", force)
			f.upgradeFrom.force = force
			if err = f.upgradeChanged(); err != nil {
				return err
			}
		case <-f.wantResolved:
			log.Debugf("worker/uniter/filter: want resolved event")
			if f.resolved != state.ResolvedNone {
				f.outResolved = f.outResolvedOn
			}
		case <-f.clearResolved:
			log.Debugf("worker/uniter/filter: resolved event handled")
			f.outResolved = nil
			if err := f.unit.ClearResolved(); err != nil {
				return err
			}
			if err = f.unitChanged(); err != nil {
				return err
			}
			select {
			case <-f.tomb.Dying():
				return tomb.ErrDying
			case f.didClearResolved <- nothing:
			}
		case <-discardConfig:
			log.Debugf("worker/uniter/filter: discarded config event")
			f.outConfig = nil
		}
	}
	panic("unreachable")
}

// unitChanged responds to changes in the unit.
func (f *filter) unitChanged() error {
	if err := f.unit.Refresh(); err != nil {
		if errors.IsNotFoundError(err) {
			return worker.ErrTerminateAgent
		}
		return err
	}
	if f.life != f.unit.Life() {
		switch f.life = f.unit.Life(); f.life {
		case state.Dying:
			log.Noticef("worker/uniter/filter: unit is dying")
			close(f.outUnitDying)
			f.outUpgrade = nil
		case state.Dead:
			log.Noticef("worker/uniter/filter: unit is dead")
			return worker.ErrTerminateAgent
		}
	}
	if resolved := f.unit.Resolved(); resolved != f.resolved {
		f.resolved = resolved
		if f.resolved != state.ResolvedNone {
			f.outResolved = f.outResolvedOn
		}
	}
	return nil
}

// serviceChanged responds to changes in the service.
func (f *filter) serviceChanged() error {
	if err := f.service.Refresh(); err != nil {
		if errors.IsNotFoundError(err) {
			return fmt.Errorf("service unexpectedly removed")
		}
		return err
	}
	url, force := f.service.CharmURL()
	f.upgradeAvailable = serviceCharm{url, force}
	switch f.service.Life() {
	case state.Dying:
		if err := f.unit.Destroy(); err != nil {
			return err
		}
	case state.Dead:
		return fmt.Errorf("service unexpectedly dead")
	}
	return f.upgradeChanged()
}

// upgradeChanged responds to changes in the service or in the
// upgrade requests that defines which charm changes should be
// delivered as upgrades.
func (f *filter) upgradeChanged() (err error) {
	if f.life != state.Alive {
		log.Debugf("worker/uniter/filter: charm check skipped, unit is dying")
		f.outUpgrade = nil
		return nil
	}
	if f.upgradeFrom.url == nil {
		log.Debugf("worker/uniter/filter: charm check skipped, not yet installed.")
		f.outUpgrade = nil
		return nil
	}
	if *f.upgradeAvailable.url != *f.upgradeFrom.url {
		if f.upgradeAvailable.force || !f.upgradeFrom.force {
			log.Debugf("worker/uniter/filter: preparing new upgrade event")
			if f.upgrade == nil || *f.upgrade != *f.upgradeAvailable.url {
				f.upgrade = f.upgradeAvailable.url
			}
			f.outUpgrade = f.outUpgradeOn
			return nil
		}
	}
	log.Debugf("worker/uniter/filter: no new charm event")
	f.outUpgrade = nil
	return nil
}

// relationsChanged responds to service relation changes.
func (f *filter) relationsChanged(ids []int) {
outer:
	for _, id := range ids {
		for _, existing := range f.relations {
			if id == existing {
				continue outer
			}
		}
		f.relations = append(f.relations, id)
	}
	if len(f.relations) != 0 {
		sort.Ints(f.relations)
		f.outRelations = f.outRelationsOn
	}
}

// serviceCharm holds information about a charm.
type serviceCharm struct {
	url   *charm.URL
	force bool
}

// nothing is marginally more pleasant to read than "struct{}{}".
var nothing = struct{}{}
