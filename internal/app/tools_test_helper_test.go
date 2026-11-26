package app

import (
	"time"

	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/filesystem"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
)

func registerCoreTools(broker *toolbroker.Broker, in *toolExecutorInput, emailAdapters ...*email.Adapter) error {
	toolsCfg := in.result.Config.Tools
	shellMod, err := shellexec.New(&shellexec.Deps{
		Workspace: toolsCfg.Workspace,
		Config:    toolsCfg.Exec,
	})
	if err != nil {
		return err
	}
	if err := broker.RegisterProvider(shellMod); err != nil {
		return err
	}
	fsMod, err := filesystem.New(&filesystem.Deps{
		Workspace: toolsCfg.Workspace,
	})
	if err != nil {
		return err
	}
	if err := broker.RegisterProvider(fsMod); err != nil {
		return err
	}
	fetchMod, err := fetch.New(&fetch.Deps{
		Config: &toolsCfg.Fetch,
	})
	if err != nil {
		return err
	}
	if err := broker.RegisterProvider(fetchMod); err != nil {
		return err
	}
	searchMod, err := search.New(&search.Deps{
		Config:       &toolsCfg.WebSearch,
		Timeout:      time.Duration(toolsCfg.Fetch.Timeout),
		MaxBodyBytes: toolsCfg.Fetch.MaxDecodedBytes,
	})
	if err != nil {
		return err
	}
	if err := broker.RegisterProvider(searchMod); err != nil {
		return err
	}
	return broker.RegisterProvider(email.NewToolProvider(emailAdapters...))
}

func newTestToolExecutor(in *toolExecutorInput, emailAdapters ...*email.Adapter) (*runtimeadk.BrokerExecutor, error) {
	broker, executor, _, err := newToolBroker(in)
	if err != nil {
		return nil, err
	}
	if err := registerCoreTools(broker, in, emailAdapters...); err != nil {
		return nil, err
	}
	return executor, nil
}
