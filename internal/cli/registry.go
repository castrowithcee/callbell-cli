package cli

import (
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/provider/bookstack"
	"github.com/castrowithcee/callbell-cli/internal/provider/lexware"
	"github.com/castrowithcee/callbell-cli/internal/provider/seatable"
	"github.com/castrowithcee/callbell-cli/internal/provider/telegram"
	"github.com/castrowithcee/callbell-cli/internal/provider/twentycrm"
)

// defaultRegistry wires every provider implementation this build ships. Registration is static, so a
// failure here is a programming error rather than a runtime condition; TestDefaultRegistry proves it.
func defaultRegistry() *capability.Registry {
	reg := capability.NewRegistry()
	if err := bookstack.Register(reg); err != nil {
		panic("provider registration is static and must not fail: " + err.Error())
	}
	if err := telegram.Register(reg); err != nil {
		panic("provider registration is static and must not fail: " + err.Error())
	}
	if err := lexware.Register(reg); err != nil {
		panic("provider registration is static and must not fail: " + err.Error())
	}
	if err := twentycrm.Register(reg); err != nil {
		panic("provider registration is static and must not fail: " + err.Error())
	}
	if err := seatable.Register(reg); err != nil {
		panic("provider registration is static and must not fail: " + err.Error())
	}
	return reg
}
