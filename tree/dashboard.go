package tree

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/lodastack/registry/common"
	"github.com/lodastack/registry/model"
)

var (
	dashboardType = "dashboard"
)

// DashboardInf is interface the dashboard resource have.
type DashboardInf interface {
	// GetDashboard return dashboard map of the ns.
	GetDashboard(ns string) (model.DashboardData, error)

	// SetDashboard set the dashboard map to the ns.
	SetDashboard(ns string, dashboardData model.DashboardData) error

	// AddDashboard add the dashboard to the ns.
	AddDashboard(ns string, dashboardData model.Dashboard) error

	// RemoveDashboard update the dashboard of the ns.
	RemoveDashboard(ns string, dIndex int) error

	// UpdateDashboard update the title of dashboard.
	UpdateDashboard(ns string, dIndex int, title string) error

	PanelInf
}

// PanelInf is the panel method.
// Panel is picture a dashboard have.
type PanelInf interface {
	// ReorderPanel update the panel order of a dashboard.
	ReorderPanel(ns string, dIndex int, newOrder []int) error

	// AddPanel add the panel to the dashboard.
	AddPanel(ns string, dIndex int, panel model.Panel) error

	// RemovePanel delete the panel of the dashboard.
	RemovePanel(ns string, dIndex int, panelIndex int) error

	// UpdatePanel update the panel of the dashboard.
	UpdatePanel(ns string, dIndex int, panelIndex int, title, graphType string) error

	// AppendTarget append a target to panel.
	AppendTarget(ns string, dIndex int, panelIndex int, target model.Target) error

	// UpdateTarget update a target.
	UpdateTarget(ns string, dIndex int, panelIndex, targetIndex int, target model.Target) error

	// RemoveTarget delete a target.
	RemoveTarget(ns string, dIndex int, panelIndex, targetIndex int) error
}

// GetDashboard return the dashboard under the ns.
func (t *Tree) GetDashboard(ns string) (model.DashboardData, error) {
	nodeID, err := t.getNodeIDByNS(ns)
	if err != nil {
		t.logger.Errorf("getIDByNs fail: %s", err.Error())
		return nil, err
	}

	resByte, err := t.getByteFromStore(nodeID, dashboardType)
	if err != nil {
		return nil, err
	}
	if len(resByte) == 0 {
		return nil, nil
	}
	var rl []model.Dashboard
	err = json.Unmarshal(resByte, &rl)
	if err != nil {
		t.logger.Errorf("unmarshal resource fail, error: %s, data: %s:", err, string(resByte))
		return nil, err
	}
	return rl, nil
}

// SetDashboard set the dashboard to a node.
func (t *Tree) SetDashboard(ns string, dashboards model.DashboardData) error {
	nodeID, err := t.getNodeIDByNS(ns)
	if err != nil {
		t.logger.Errorf("getIDByNs fail: %s", err.Error())
		return err
	}
	resNewByte, err := json.Marshal(dashboards)
	if err != nil {
		t.logger.Errorf("marshal dashboard fail: %s", err.Error())
		return err
	}
	return t.setByteToStore(nodeID, dashboardType, resNewByte)
}

// AddDashboard add a dashboard to a ns.
func (t *Tree) AddDashboard(ns string, dashboardData model.Dashboard) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil {
		return err
	}

	dashboards = append(dashboards, dashboardData)
	return t.SetDashboard(ns, dashboards)
}

// UpdateDashboard update one dashboard title of ns.
func (t *Tree) UpdateDashboard(ns string, dIndex int, title string) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil {
		return err
	}
	if dIndex >= len(dashboards) {
		return common.ErrInvalidParam
	}
	dashboards[dIndex].Title = title
	return t.SetDashboard(ns, dashboards)
}

// RemoveDashboard one dashboard of ns.
func (t *Tree) RemoveDashboard(ns string, dIndex int) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || dIndex >= len(dashboards) {
		t.logger.Errorf("DeleteDashboard error, data: %+v, error: %v", dashboards, err)
		return err
	}

	copy(dashboards[dIndex:], dashboards[dIndex+1:])
	return t.SetDashboard(ns, dashboards[:len(dashboards)-1])
}

// ReorderPanel update the order of panel by newOrder.
func (t *Tree) ReorderPanel(ns string, dIndex int, newOrder []int) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) {
		t.logger.Errorf("ReorderPanel error, data: %+v, error: %v", dashboards, err)
		return common.ErrInvalidParam
	}
	if len(dashboards[dIndex].Panels) != len(newOrder) {
		return errors.New("dashboard name or new order invalid")
	}
	if invalidOrder(newOrder) {
		return errors.New("dashboard new order invalid")
	}

	newPanels := make([]model.Panel, len(dashboards[dIndex].Panels))
	for i, order := range newOrder {
		newPanels[i] = dashboards[dIndex].Panels[order]
	}
	dashboards[dIndex].Panels = newPanels
	return t.SetDashboard(ns, dashboards)
}

// AddPanel add a panel to a dashboard.
func (t *Tree) AddPanel(ns string, dIndex int, panel model.Panel) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) {
		t.logger.Errorf("AddPanel error, data: %+v, error: %v", dashboards, err)
		return common.ErrInvalidParam
	}

	dashboards[dIndex].Panels = append(dashboards[dIndex].Panels, panel)
	return t.SetDashboard(ns, dashboards)
}

// UpdatePanel update a panel.
func (t *Tree) UpdatePanel(ns string, dIndex int, panelIndex int, title, graphType string) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) || len(dashboards[dIndex].Panels) <= panelIndex {
		t.logger.Errorf("AddPanel error, data: %+v, dindex %d, pindex %d, error: %v", dashboards, dIndex, panelIndex, err)
		return common.ErrInvalidParam
	}

	if title != "" {
		dashboards[dIndex].Panels[panelIndex].Title = title
	}
	if graphType != "" {
		dashboards[dIndex].Panels[panelIndex].GraphType = graphType
	}
	return t.SetDashboard(ns, dashboards)
}

// RemovePanel remove a panel from a dashboard.
func (t *Tree) RemovePanel(ns string, dIndex int, panelIndex int) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) || panelIndex >= len(dashboards[dIndex].Panels) {
		t.logger.Errorf("AddPanel error, data: %+v, dindex %d, pindex %d, error: %v", dashboards, dIndex, panelIndex, err)
		return common.ErrInvalidParam
	}

	// TODO: check
	copy(dashboards[dIndex].Panels[panelIndex:], dashboards[dIndex].Panels[panelIndex+1:])
	dashboards[dIndex].Panels = dashboards[dIndex].Panels[:len(dashboards[dIndex].Panels)-1]
	return t.SetDashboard(ns, dashboards)
}

func invalidOrder(order sort.IntSlice) bool {
	tmp := make(sort.IntSlice, len(order))
	copy(tmp, order)
	tmp.Sort()
	for i, index := range tmp {
		if i != index {
			return true
		}
	}
	return false
}

// AppendTarget append a target to panel.
func (t *Tree) AppendTarget(ns string, dIndex int, panelIndex int, target model.Target) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) || panelIndex >= len(dashboards[dIndex].Panels) {
		t.logger.Errorf("AddPanel error, data: %+v, dindex %d, pindex %d, error: %v", dashboards, dIndex, panelIndex, err)
		return common.ErrInvalidParam
	}

	dashboards[dIndex].Panels[panelIndex].Targets = append(dashboards[dIndex].Panels[panelIndex].Targets, target)
	return t.SetDashboard(ns, dashboards)
}

// UpdateTarget update a target.
func (t *Tree) UpdateTarget(ns string, dIndex int, panelIndex, targetIndex int, target model.Target) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) || panelIndex >= len(dashboards[dIndex].Panels) || targetIndex >= len(dashboards[dIndex].Panels[panelIndex].Targets) {
		t.logger.Errorf("AddPanel error, data: %+v, dindex %d, pindex %d, error: %v", dashboards, dIndex, panelIndex, err)
		return common.ErrInvalidParam
	}

	dashboards[dIndex].Panels[panelIndex].Targets[targetIndex] = target
	return t.SetDashboard(ns, dashboards)
}

// RemoveTarget remove update a target.
func (t *Tree) RemoveTarget(ns string, dIndex int, panelIndex, targetIndex int) error {
	dashboards, err := t.GetDashboard(ns)
	if err != nil || len(dashboards) == 0 || dIndex >= len(dashboards) || panelIndex >= len(dashboards[dIndex].Panels) || targetIndex >= len(dashboards[dIndex].Panels[panelIndex].Targets) {
		t.logger.Errorf("AddPanel error, data: %+v, dindex %d, pindex %d, error: %v", dashboards, dIndex, panelIndex, err)
		return common.ErrInvalidParam
	}
	if targetIndex+1 < len(dashboards[dIndex].Panels[panelIndex].Targets) {
		copy(dashboards[dIndex].Panels[panelIndex].Targets[targetIndex:], dashboards[dIndex].Panels[panelIndex].Targets[targetIndex+1:])
	}
	length := len(dashboards[dIndex].Panels[panelIndex].Targets)
	dashboards[dIndex].Panels[panelIndex].Targets = dashboards[dIndex].Panels[panelIndex].Targets[:length-1]

	return t.SetDashboard(ns, dashboards)
}
