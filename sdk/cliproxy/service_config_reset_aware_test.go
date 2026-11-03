package cliproxy

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestResetAwareRoutingSelectorCarriesTheReserveFromYAML(t *testing.T) {
	cfg, err := internalconfig.ParseConfigBytes([]byte(`
routing:
  strategy: "reset-aware"
  session-affinity: true
  session-affinity-rebind-on: "quota"
  reset-aware-reserve:
    account: "eugen.eistrach@gmail.com"
    percent: 20
    release-before-reset: "12h"
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	state := normalizedRoutingRuntimeState(cfg)
	want := coreauth.ResetAwareReserve{Account: "eugen.eistrach@gmail.com", Share: 0.2, ReleaseBefore: 12 * time.Hour}
	if state.resetAwareReserve != want {
		t.Fatalf("reserve = %+v, want %+v", state.resetAwareReserve, want)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type = %T, want session affinity around reset-aware", newRoutingSelector(state))
	}
	if state == normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "reset-aware", SessionAffinity: true, SessionAffinityRebindOn: "quota"}}) {
		t.Fatal("changing only the reserve must count as a routing change, or a config reload keeps the old selector")
	}
}
