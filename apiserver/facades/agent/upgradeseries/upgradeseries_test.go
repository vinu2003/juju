// Copyright 2018 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package upgradeseries_test

import (
	"github.com/golang/mock/gomock"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/common/mocks"
	"github.com/juju/juju/apiserver/facades/agent/upgradeseries"
	"github.com/juju/juju/apiserver/params"
	apiservertesting "github.com/juju/juju/apiserver/testing"
	"github.com/juju/juju/core/model"
	"github.com/juju/juju/state"
	"github.com/juju/juju/testing"
)

type upgradeSeriesSuite struct {
	testing.BaseSuite

	machineTag names.MachineTag
	unitTag    names.UnitTag
}

var _ = gc.Suite(&upgradeSeriesSuite{})

func (s *upgradeSeriesSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)

	s.machineTag = names.NewMachineTag("0")
	s.unitTag = names.NewUnitTag("redis/0")
}

func (s *upgradeSeriesSuite) TestMachineStatus(c *gc.C) {
	ctrl := gomock.NewController(c)
	defer ctrl.Finish()

	api, backend := s.newAPI(c, ctrl)
	machine := mocks.NewMockUpgradeSeriesMachine(ctrl)

	backend.EXPECT().Machine(s.machineTag.Id()).Return(machine, nil)
	machine.EXPECT().MachineUpgradeSeriesStatus().Return(model.PrepareCompleted, nil)

	entity := params.Entity{Tag: s.machineTag.String()}
	args := params.Entities{
		Entities: []params.Entity{entity},
	}

	results, err := api.MachineStatus(args)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.DeepEquals, params.UpgradeSeriesStatusResultsNew{
		Results: []params.UpgradeSeriesStatusResultNew{
			{
				Status: params.UpgradeSeriesStatus{Entity: entity, Status: model.PrepareCompleted},
			},
		},
	})
}

func (s *upgradeSeriesSuite) TestSetMachineStatus(c *gc.C) {
	ctrl := gomock.NewController(c)
	defer ctrl.Finish()

	api, backend := s.newAPI(c, ctrl)
	machine := mocks.NewMockUpgradeSeriesMachine(ctrl)

	backend.EXPECT().Machine(s.machineTag.Id()).Return(machine, nil)
	machine.EXPECT().SetMachineUpgradeSeriesStatus(model.PrepareCompleted).Return(nil)

	entity := params.Entity{Tag: s.machineTag.String()}
	args := params.UpgradeSeriesStatusParams{
		Params: []params.UpgradeSeriesStatus{{Entity: entity, Status: model.PrepareCompleted}},
	}

	results, err := api.SetMachineStatus(args)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.DeepEquals, params.ErrorResults{
		Results: []params.ErrorResult{{}},
	})
}

func (s *upgradeSeriesSuite) TestUnitsPrepared(c *gc.C) {
	ctrl := gomock.NewController(c)
	defer ctrl.Finish()

	api, backend := s.newAPI(c, ctrl)
	machine := mocks.NewMockUpgradeSeriesMachine(ctrl)

	backend.EXPECT().Machine(s.machineTag.Id()).Return(machine, nil)
	machine.EXPECT().UpgradeSeriesUnitStatuses().Return(map[string]state.UpgradeSeriesUnitStatus{
		"redis/0": {Status: model.PrepareCompleted},
		"redis/1": {Status: model.PrepareStarted},
	}, nil)

	args := params.Entities{Entities: []params.Entity{{Tag: s.machineTag.String()}}}

	results, err := api.UnitsPrepared(args)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.DeepEquals, params.EntitiesResults{
		Results: []params.EntitiesResult{{Entities: []params.Entity{{Tag: s.unitTag.String()}}}},
	})
}

func (s *upgradeSeriesSuite) TestUnitsCompleted(c *gc.C) {
	ctrl := gomock.NewController(c)
	defer ctrl.Finish()

	api, backend := s.newAPI(c, ctrl)
	machine := mocks.NewMockUpgradeSeriesMachine(ctrl)

	backend.EXPECT().Machine(s.machineTag.Id()).Return(machine, nil)
	machine.EXPECT().UpgradeSeriesUnitStatuses().Return(map[string]state.UpgradeSeriesUnitStatus{
		"redis/0": {Status: model.Completed},
		"redis/1": {Status: model.CompleteStarted},
	}, nil)

	args := params.Entities{Entities: []params.Entity{{Tag: s.machineTag.String()}}}

	results, err := api.UnitsCompleted(args)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.DeepEquals, params.EntitiesResults{
		Results: []params.EntitiesResult{{Entities: []params.Entity{{Tag: s.unitTag.String()}}}},
	})
}

func (s *upgradeSeriesSuite) newAPI(
	c *gc.C, ctrl *gomock.Controller,
) (*upgradeseries.API, *mocks.MockUpgradeSeriesBackend) {
	resources := common.NewResources()
	authorizer := apiservertesting.FakeAuthorizer{
		Tag: s.machineTag,
	}

	mockBackend := mocks.NewMockUpgradeSeriesBackend(ctrl)

	api, err := upgradeseries.NewUpgradeSeriesAPI(mockBackend, resources, authorizer)
	c.Assert(err, jc.ErrorIsNil)

	return api, mockBackend
}
