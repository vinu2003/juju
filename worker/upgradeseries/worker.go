// Copyright 2018 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package upgradeseries

import (
	"strings"

	"github.com/juju/errors"
	"gopkg.in/juju/names.v2"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/juju/worker.v1/catacomb"

	"github.com/juju/juju/core/model"
	"github.com/juju/juju/service"
)

// TODO (manadart 2018-07-30) Relocate this somewhere more central?
//go:generate mockgen -package mocks -destination mocks/worker_mock.go gopkg.in/juju/worker.v1 Worker

//go:generate mockgen -package mocks -destination mocks/package_mock.go github.com/juju/juju/worker/upgradeseries Facade,Logger,AgentService,ServiceAccess

// Logger represents the methods required to emit log messages.
type Logger interface {
	Debugf(message string, args ...interface{})
	Infof(message string, args ...interface{})
	Warningf(message string, args ...interface{})
	Errorf(message string, args ...interface{})
}

// Config is the configuration needed to constuct an UpgradeSeries worker.
type Config struct {
	// FacadeFactory is used to acquire back-end state with
	// the input tag context.
	FacadeFactory func(names.Tag) Facade

	// Logger is the logger for this worker.
	Logger Logger

	// Tag is the current machine tag.
	Tag names.Tag

	// ServiceAccess provides access to the local init system.
	Service ServiceAccess
}

// Validate validates the upgrade-series worker configuration.
func (config Config) Validate() error {
	if config.Logger == nil {
		return errors.NotValidf("nil Logger")
	}
	if config.Tag == nil {
		return errors.NotValidf("nil machine tag")
	}
	k := config.Tag.Kind()
	if k != names.MachineTagKind {
		return errors.NotValidf("%q tag kind", k)
	}
	if config.FacadeFactory == nil {
		return errors.NotValidf("nil FacadeFactory")
	}
	if config.Service == nil {
		return errors.NotValidf("nil Service")
	}
	return nil
}

// upgradeSeriesWorker is responsible for machine and unit agent requirements
// during upgrade-series:
// 		copying the agent binary directory and renaming;
// 		rewriting the machine and unit(s) systemd files if necessary;
// 		stopping the unit agents;
//		starting the unit agents;
//		moving the status of the upgrade-series steps along.
type upgradeSeriesWorker struct {
	Facade

	facadeFactory func(names.Tag) Facade
	catacomb      catacomb.Catacomb
	logger        Logger
	service       ServiceAccess
}

// NewWorker creates, starts and returns a new upgrade-series worker based on
// the input configuration.
func NewWorker(config Config) (worker.Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}

	w := &upgradeSeriesWorker{
		Facade:        config.FacadeFactory(config.Tag),
		facadeFactory: config.FacadeFactory,
		logger:        config.Logger,
		service:       config.Service,
	}

	if err := catacomb.Invoke(catacomb.Plan{
		Site: &w.catacomb,
		Work: w.loop,
	}); err != nil {
		return nil, errors.Trace(err)
	}

	return w, nil
}

func (w *upgradeSeriesWorker) loop() error {
	uw, err := w.WatchUpgradeSeriesNotifications()
	if err != nil {
		return errors.Trace(err)
	}
	err = w.catacomb.Add(uw)
	if err != nil {
		return errors.Trace(err)
	}
	for {
		select {
		case <-w.catacomb.Dying():
			return w.catacomb.ErrDying()
		case <-uw.Changes():
			if err := w.handleUpgradeSeriesChange(); err != nil {
				return errors.Trace(err)
			}
		}
	}
}

// handleUpgradeSeriesChange retrieves the current upgrade-series status for
// this machine and based on the status, calls methods that will progress
// the workflow accordingly.
func (w *upgradeSeriesWorker) handleUpgradeSeriesChange() error {
	machineStatus, err := w.MachineStatus()
	if err != nil {
		if errors.IsNotFound(err) {
			// No upgrade-series lock.
			// This can only happen on the first watch call.
			// Does it happen if the lock is deleted?
			w.logger.Warningf("no series upgrade lock present")
			return nil
		}
		return errors.Trace(err)
	}
	w.logger.Debugf("series upgrade lock changed")

	switch machineStatus {
	case model.PrepareStarted:
		err = w.handlePrepareStarted()
	case model.PrepareMachine:
		err = w.handlePrepareMachine()
	case model.CompleteStarted:
		err = w.handleCompleteStarted()
	default:
		w.logger.Debugf("machine series upgrade status is %q", machineStatus)
	}
	return errors.Trace(err)
}

// handlePrepareStarted handles workflow for the machine with an upgrade-series
// lock status of "PrepareStarted"
func (w *upgradeSeriesWorker) handlePrepareStarted() error {
	w.logger.Debugf("machine series upgrade status is %q", model.PrepareStarted)

	units, allConfirmed, err := w.compareUnitAgentServices(w.UnitsPrepared)
	if err != nil {
		return errors.Trace(err)
	}
	if !allConfirmed {
		w.logger.Debugf(
			"still waiting for units to complete series upgrade preparation; known unit agent services:\n\t%s",
			unitNames(units),
		)
		return nil
	}

	return errors.Trace(w.transitionPrepareMachine(units))
}

// transitionPrepareMachine stops all unit agents on this machine and updates
// the upgrade-series status lock to indicate that upgrade work can proceed.
// TODO (manadart 2018-08-09): Rename when a better name is contrived for
// "UpgradeSeriesPrepareMachine".
func (w *upgradeSeriesWorker) transitionPrepareMachine(unitServices map[string]string) error {
	w.logger.Infof("stopping units for series upgrade")

	for unit, serviceName := range unitServices {
		svc, err := w.service.DiscoverService(serviceName)
		if err != nil {
			return errors.Trace(err)
		}
		running, err := svc.Running()
		if err != nil {
			return errors.Trace(err)
		}
		if !running {
			continue
		}

		if err := svc.Stop(); err != nil {
			return errors.Annotatef(err, "stopping %q unit agent for series upgrade", unit)
		}
	}

	return errors.Trace(w.SetMachineStatus(model.PrepareMachine))
}

// handlePrepareMachine handles workflow for the machine with an upgrade-series
// lock status of "PrepareMachine".
// TODO (manadart 2018-08-09): Rename when a better name is contrived for
// "UpgradeSeriesPrepareMachine".
func (w *upgradeSeriesWorker) handlePrepareMachine() error {
	w.logger.Debugf("machine series upgrade status is %q", model.PrepareMachine)

	// This is a sanity check.
	// The units should all still be in the "PrepareComplete" state.
	units, allConfirmed, err := w.compareUnitAgentServices(w.UnitsPrepared)
	if err != nil {
		return errors.Trace(err)
	}
	if !allConfirmed {
		w.logger.Debugf(
			"units are not all in the expected state for series upgrade preparation (complete); "+
				"known unit agent services:\n\t%s",
			unitNames(units),
		)
	}

	return errors.Trace(w.transitionPrepareComplete(units))
}

// transitionPrepareComplete rewrites service unit files for unit agents running
// on this machine so that they are compatible with the init system of the
// series upgrade target
func (w *upgradeSeriesWorker) transitionPrepareComplete(unitServices map[string]string) error {
	w.logger.Infof("preparing service units for series upgrade")

	// TODO (manadart 2018-08-09): Unit file wrangling to come.
	// For now we just update the machine status to progress the workflow.
	return errors.Trace(w.SetMachineStatus(model.PrepareCompleted))
}

func (w *upgradeSeriesWorker) handleCompleteStarted() error {
	w.logger.Debugf("machine series upgrade status is %q", model.CompleteStarted)

	// If the units are still all in the "PrepareComplete" state, then the
	// manual tasks have been run and an operator has executed the
	// upgrade-series completion command; start all the unit agents,
	// and progress the workflow.
	units, allConfirmed, err := w.compareUnitAgentServices(w.UnitsPrepared)
	if err != nil {
		return errors.Trace(err)
	}
	if allConfirmed {
		return errors.Trace(w.transitionUnitsStarted(units))
	}

	// If the units have all completed their workflow, then we are done.
	// Make the final update to the lock to say the machine is completed.
	units, allConfirmed, err = w.compareUnitAgentServices(w.UnitsCompleted)
	if err != nil {
		return errors.Trace(err)
	}
	if allConfirmed {
		w.logger.Infof("series upgrade complete")
		return errors.Trace(w.SetMachineStatus(model.Completed))
	}

	return nil
}

// transitionUnitsStarted iterates over units managed by this machine. Starts
// the unit's agent service, and transitions all unit subordinate statuses.
func (w *upgradeSeriesWorker) transitionUnitsStarted(unitServices map[string]string) error {
	w.logger.Infof("starting units after series upgrade")

	for unit, serviceName := range unitServices {
		svc, err := w.service.DiscoverService(serviceName)
		if err != nil {
			return errors.Trace(err)
		}
		running, err := svc.Running()
		if err != nil {
			return errors.Trace(err)
		}
		if running {
			continue
		}
		if err := svc.Start(); err != nil {
			return errors.Annotatef(err, "starting %q unit agent after series upgrade", unit)
		}
	}

	return errors.Trace(w.StartUnitCompletion())
}

// unitsInState is a type alias for retrieving a slice of unit tags for units
// in a particular state.
type unitsInState = func() ([]names.UnitTag, error)

// compareUnitsAgentServices executes the input getter method to retrieve a
// collection of unit tags.
// It then filters the services running on the local machine to those that are
// for unit agents.
// The service names keyed by unit names are returned, along with a boolean
// indicating whether all the retrieved unit tags are represented in the
// service map.
// NOTE: No unit tags and no agent services returns true, meaning that the
// workflow can progress.
func (w *upgradeSeriesWorker) compareUnitAgentServices(getUnits unitsInState) (map[string]string, bool, error) {
	units, err := getUnits()
	if err != nil {
		return nil, false, errors.Trace(err)
	}

	services, err := w.service.ListServices()
	if err != nil {
		return nil, false, errors.Trace(err)
	}

	unitServices := service.FindUnitServiceNames(services)
	if len(units) != len(unitServices) {
		return unitServices, false, nil
	}

	for _, u := range units {
		if _, ok := unitServices[u.Id()]; !ok {
			return unitServices, false, nil
		}
	}
	return unitServices, true, nil
}

func unitsCompleted(statuses []string) bool {
	return unitsAllWithStatus(statuses, model.Completed)
}

func unitsAllWithStatus(statuses []string, status model.UpgradeSeriesStatus) bool {
	t := string(status)
	for _, s := range statuses {
		if s != t {
			return false
		}
	}
	return true
}

// Kill implements worker.Worker.Kill.
func (w *upgradeSeriesWorker) Kill() {
	w.catacomb.Kill(nil)
}

// Wait implements worker.Worker.Wait.
func (w *upgradeSeriesWorker) Wait() error {
	return w.catacomb.Wait()
}

// Stop stops the upgrade-series worker and returns any
// error it encountered when running.
func (w *upgradeSeriesWorker) Stop() error {
	w.Kill()
	return w.Wait()
}

// unitNames returns a comma-delimited string of unit names based on the input
// map of unit agent services.
func unitNames(units map[string]string) string {
	unitIds := make([]string, len(units))
	i := 0
	for u := range units {
		unitIds[i] = u
		i++
	}
	return strings.Join(unitIds, ", ")
}
