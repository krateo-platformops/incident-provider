// Package controllers sets up the controllers of this manager.
package controllers

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/krateo-platformops/incident-provider/internal/controllers/incident"
)

// Setup adds every controller to the manager.
func Setup(mgr ctrl.Manager, o incident.Options, a incident.ApplyOptions) error {
	if err := incident.Setup(mgr, o); err != nil {
		return err
	}
	if err := incident.SetupClosed(mgr, o); err != nil {
		return err
	}
	return incident.SetupApply(mgr, a)
}
