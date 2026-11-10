package tgclient

import (
	"context"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
)

func TestWithConnectDefaultsDialsStoredDCsFirst(t *testing.T) {
	ctx := context.Background()
	storage := &session.StorageMemory{}
	stored := tg.DCOption{ID: 2, IPAddress: "149.154.167.51", Port: 443}
	err := (&session.Loader{Storage: storage}).Save(ctx, &session.Data{
		DC:      2,
		Config:  session.Config{DCOptions: []tg.DCOption{stored}},
		AuthKey: make([]byte, 256),
	})
	if err != nil {
		t.Fatalf("saving session: %v", err)
	}

	opts := withConnectDefaults(ctx, storage, telegram.Options{})

	if opts.DialTimeout != dialTimeout {
		t.Errorf("DialTimeout = %s, want %s", opts.DialTimeout, dialTimeout)
	}
	got := dcs.FindPrimaryDCs(opts.DCList.Options, 2, false)
	if len(got) < 2 || got[0].IPAddress != stored.IPAddress {
		t.Fatalf("DC 2 candidates = %+v, want the stored address first and the built-in ones after it", got)
	}
	builtIn := dcs.FindPrimaryDCs(dcs.Prod().Options, 2, false)[0].IPAddress
	found := false
	for _, o := range got {
		found = found || o.IPAddress == builtIn
	}
	if !found {
		t.Errorf("DC 2 candidates = %+v, want the built-in %s kept as a fallback", got, builtIn)
	}
}

func TestWithConnectDefaultsWithoutSessionKeepsGotdDefaults(t *testing.T) {
	opts := withConnectDefaults(context.Background(), &session.StorageMemory{}, telegram.Options{})
	if !opts.DCList.Zero() {
		t.Errorf("DCList = %+v, want it left to gotd when there is no session", opts.DCList)
	}
}

func TestWithConnectDefaultsKeepsExplicitList(t *testing.T) {
	list := dcs.List{Options: []tg.DCOption{{ID: 1, IPAddress: "127.0.0.1", Port: 1}}}
	opts := withConnectDefaults(context.Background(), &session.StorageMemory{}, telegram.Options{DCList: list})
	if len(opts.DCList.Options) != 1 || opts.DCList.Options[0].IPAddress != "127.0.0.1" {
		t.Errorf("DCList = %+v, want the caller's list untouched", opts.DCList)
	}
}

func TestMergeDCOptionsDropsDuplicates(t *testing.T) {
	a := tg.DCOption{ID: 2, IPAddress: "149.154.167.41", Port: 443}
	media := tg.DCOption{ID: 2, IPAddress: "149.154.167.41", Port: 443, MediaOnly: true}
	b := tg.DCOption{ID: 2, IPAddress: "149.154.167.51", Port: 443}

	got := mergeDCOptions([]tg.DCOption{b, a}, []tg.DCOption{a, media})

	if len(got) != 3 || got[0].IPAddress != b.IPAddress || got[1].IPAddress != a.IPAddress || got[1].MediaOnly || !got[2].MediaOnly {
		t.Errorf("merged = %+v, want [b a media]", got)
	}
}
