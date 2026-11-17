package governance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStandaloneStore builds a LocalGovernanceStore with no config store /
// persistence — just the in-memory maps. Enough for exercising the CAS
// primitives without going through GovernanceConfig preload paths.
func newStandaloneStore(t *testing.T) *LocalGovernanceStore {
	t.Helper()
	return &LocalGovernanceStore{
		logger:                         NewMockLogger(),
		LastDBUsagesBudgets:            map[string]float64{},
		LastDBUsagesTokensRateLimits:   map[string]int64{},
		LastDBUsagesRequestsRateLimits: map[string]int64{},
	}
}

// TestGetVirtualKeyByID exercises the ID-keyed lookup directly without taking
// the full governance snapshot path.
func TestGetVirtualKeyByID(t *testing.T) {
	store := newStandaloneStore(t)
	vk := &configstoreTables.TableVirtualKey{ID: "vk-id", Name: "test", Value: *schemas.NewSecretVar("sk-bf-test")}
	store.storeVirtualKey(vk.Value.GetValue(), vk)

	got, found := store.GetVirtualKeyByID(context.Background(), vk.ID)
	require.True(t, found)
	assert.Same(t, vk, got)

	got, found = store.GetVirtualKeyByID(context.Background(), "missing")
	assert.False(t, found)
	assert.Nil(t, got)
}

// TestBumpBudgetUsage_NoLostIncrements proves the CAS retry loop in
// BumpBudgetUsage never drops a concurrent increment. Without the CAS, the
// Load→clone→mutate→Store sequence races and the final CurrentUsage ends up
// strictly less than N*cost under contention.
func TestBumpBudgetUsage_NoLostIncrements(t *testing.T) {
	store := newStandaloneStore(t)
	budgetID := "concurrent-budget"
	store.budgets.Store(budgetID, buildBudget(budgetID, 1_000_000_000, "24h"))

	const goroutines = 256
	const perGoroutine = 50
	const cost = 1.0
	expected := float64(goroutines * perGoroutine)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				assert.NoError(t, store.BumpBudgetUsage(context.Background(), budgetID, cost))
			}
		}()
	}
	wg.Wait()

	final := store.LoadBudget(context.Background(), budgetID)
	require.NotNil(t, final)
	assert.Equal(t, expected, final.CurrentUsage, "CurrentUsage must equal total increments — any shortfall is a dropped write")
}

// TestBumpRateLimitUsage_NoLostIncrements covers the rate-limit variant of
// the same race: token and request counters are independent int64 fields
// updated on the same struct, and both must survive contention intact.
func TestBumpRateLimitUsage_NoLostIncrements(t *testing.T) {
	store := newStandaloneStore(t)
	rlID := "concurrent-rate-limit"
	store.rateLimits.Store(rlID, buildRateLimit(rlID, 1_000_000_000, 1_000_000_000))

	const goroutines = 256
	const perGoroutine = 50
	const tokensPerCall = int64(7)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				assert.NoError(t, store.BumpRateLimitUsage(context.Background(), rlID, tokensPerCall, true, true))
			}
		}()
	}
	wg.Wait()

	final := store.LoadRateLimit(context.Background(), rlID)
	require.NotNil(t, final)
	assert.Equal(t, int64(goroutines*perGoroutine)*tokensPerCall, final.TokenCurrentUsage, "TokenCurrentUsage dropped increments")
	assert.Equal(t, int64(goroutines*perGoroutine), final.RequestCurrentUsage, "RequestCurrentUsage dropped increments")
}

func TestLighthouseRateLimitVirtualKeyUpdatePreservesResetAndConcurrentUsage(t *testing.T) {
	ctx := context.Background()
	store := newStandaloneStore(t)
	rlID := "lighthouse-vk-rate-limit-race"
	initial := buildRateLimit(rlID, 10_000, 10_000)
	initial.TokenLastReset = time.Now()
	initial.RequestLastReset = initial.TokenLastReset
	vkValue := *schemas.NewSecretVar("sk-lighthouse-vk-rate-limit-race")
	store.CreateVirtualKeyInMemory(ctx, &configstoreTables.TableVirtualKey{
		ID:          "lighthouse-vk-rate-limit-race",
		Name:        "lighthouse-vk-rate-limit-race",
		Value:       vkValue,
		RateLimitID: &rlID,
		RateLimit:   initial,
	})
	require.NoError(t, store.BumpRateLimitUsageBy(ctx, rlID, 12, 3))
	previous := store.LoadRateLimit(ctx, rlID)
	require.NotNil(t, previous)

	updated := buildRateLimit(rlID, 20_000, 20_000)
	updated.TokenCurrentUsage = 0
	updated.RequestCurrentUsage = 0
	updated.TokenLastReset = time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC)
	updated.RequestLastReset = time.Date(2026, time.November, 11, 0, 0, 0, 0, time.UTC)
	updateVK := &configstoreTables.TableVirtualKey{
		ID:          "lighthouse-vk-rate-limit-race",
		Name:        "lighthouse-vk-rate-limit-race",
		Value:       vkValue,
		RateLimitID: &rlID,
		RateLimit:   updated,
	}

	var hookAttempts atomic.Int64
	var armed atomic.Bool
	armed.Store(true)
	snapshotRead := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	store.rateLimitConfigPublishHook = func() {
		hookAttempts.Add(1)
		if armed.CompareAndSwap(true, false) {
			close(snapshotRead)
			<-release
		}
	}

	updatedDone := make(chan struct{})
	go func() {
		defer close(updatedDone)
		store.UpdateVirtualKeyInMemory(ctx, updateVK, nil, nil, nil)
	}()
	<-snapshotRead // The config publisher has read the pre-bump snapshot.
	resetAt := time.Now()
	reset, didReset := store.ResetRateLimitAt(ctx, rlID, &resetAt, &resetAt)
	require.True(t, didReset, "the test reset must replace the config publisher's snapshot")
	require.NotNil(t, reset)
	require.NoError(t, store.BumpRateLimitUsage(ctx, rlID, 7, true, true))
	assert.NotSame(t, previous, store.LoadRateLimit(ctx, rlID), "the usage writer must replace the snapshot before the config commit")
	releaseOnce.Do(func() { close(release) })
	<-updatedDone

	final := store.LoadRateLimit(ctx, rlID)
	require.NotNil(t, final)
	assert.Equal(t, int64(7), final.TokenCurrentUsage, "config publication must not drop usage after reset")
	assert.Equal(t, int64(1), final.RequestCurrentUsage, "config publication must not drop requests after reset")
	assert.True(t, final.TokenLastReset.Equal(resetAt), "the token reset time must stay with the live counter")
	assert.True(t, final.RequestLastReset.Equal(resetAt), "the request reset time must stay with the live counter")
	assert.Equal(t, int64(20_000), *final.TokenMaxLimit, "the new config must still be published")
	assert.GreaterOrEqual(t, hookAttempts.Load(), int64(2), "the config publisher must retry after its stale snapshot loses the CAS")
	assert.Equal(t, int64(12), previous.TokenCurrentUsage, "the old snapshot must stay immutable")
	assert.Equal(t, int64(3), previous.RequestCurrentUsage, "the old snapshot must stay immutable")
	assert.True(t, previous.TokenLastReset.Before(resetAt), "the old reset time must stay immutable")
	assert.Equal(t, int64(0), updated.TokenCurrentUsage, "the caller config must not receive copied counters")
	assert.Equal(t, int64(0), updated.RequestCurrentUsage, "the caller config must not receive copied counters")

	rawVK, found := store.virtualKeys.Load(vkValue.GetValue())
	require.True(t, found)
	storedVK := rawVK.(*configstoreTables.TableVirtualKey)
	assert.NotSame(t, updateVK.RateLimit, storedVK.RateLimit, "the owner snapshot must be detached from the caller")
	assert.NotSame(t, final, storedVK.RateLimit, "the owner snapshot must be detached from the authoritative map")
	inputMax := *updated.TokenMaxLimit
	*updated.TokenMaxLimit = 99
	assert.Equal(t, inputMax, *final.TokenMaxLimit, "later caller pointer mutation must not change the map snapshot")
	ownerMax := *storedVK.RateLimit.TokenMaxLimit
	*storedVK.RateLimit.TokenMaxLimit = 101
	assert.Equal(t, inputMax, *final.TokenMaxLimit, "later owner pointer mutation must not change the map snapshot")
	assert.Equal(t, int64(7), final.TokenCurrentUsage)
	assert.Equal(t, ownerMax, inputMax, "owner config should contain the published value")
}

func TestLighthouseRateLimitVirtualKeyUpdatePreservesConcurrentUsage(t *testing.T) {
	ctx := context.Background()
	store := newStandaloneStore(t)
	rlID := "lighthouse-vk-rate-limit-no-reset-race"
	initial := buildRateLimit(rlID, 10_000, 10_000)
	initial.TokenLastReset = time.Now()
	initial.RequestLastReset = initial.TokenLastReset
	vkValue := *schemas.NewSecretVar("sk-lighthouse-vk-rate-limit-no-reset-race")
	store.CreateVirtualKeyInMemory(ctx, &configstoreTables.TableVirtualKey{
		ID:          rlID,
		Name:        rlID,
		Value:       vkValue,
		RateLimitID: &rlID,
		RateLimit:   initial,
	})
	require.NoError(t, store.BumpRateLimitUsageBy(ctx, rlID, 12, 3))
	previous := store.LoadRateLimit(ctx, rlID)
	require.NotNil(t, previous)
	updated := buildRateLimit(rlID, 20_000, 20_000)
	updateVK := &configstoreTables.TableVirtualKey{
		ID:          rlID,
		Name:        rlID,
		Value:       vkValue,
		RateLimitID: &rlID,
		RateLimit:   updated,
	}

	var hookAttempts atomic.Int64
	var armed atomic.Bool
	armed.Store(true)
	snapshotRead := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	store.rateLimitConfigPublishHook = func() {
		hookAttempts.Add(1)
		if armed.CompareAndSwap(true, false) {
			close(snapshotRead)
			<-release
		}
	}

	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		store.UpdateVirtualKeyInMemory(ctx, updateVK, nil, nil, nil)
	}()
	<-snapshotRead
	require.NoError(t, store.BumpRateLimitUsageBy(ctx, rlID, 7, 2))
	assert.NotSame(t, previous, store.LoadRateLimit(ctx, rlID), "the usage writer must replace the snapshot before the config commit")
	releaseOnce.Do(func() { close(release) })
	<-updateDone

	final := store.LoadRateLimit(ctx, rlID)
	require.NotNil(t, final)
	assert.Equal(t, int64(19), final.TokenCurrentUsage, "config publication must not drop the token increment")
	assert.Equal(t, int64(5), final.RequestCurrentUsage, "config publication must not drop the request increment")
	assert.True(t, final.TokenLastReset.Equal(previous.TokenLastReset))
	assert.True(t, final.RequestLastReset.Equal(previous.RequestLastReset))
	assert.Equal(t, int64(20_000), *final.TokenMaxLimit, "the new config must still be published")
	assert.GreaterOrEqual(t, hookAttempts.Load(), int64(2), "the stale config snapshot must lose its CAS and retry")
}

func TestLighthouseRateLimitOnlineOwnerPublishersPreserveDetachedSnapshots(t *testing.T) {
	ctx := context.Background()
	tokenReset := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	requestReset := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)

	type publisherCase struct {
		name        string
		id          string
		aligned     bool
		create      func(*LocalGovernanceStore, *configstoreTables.TableRateLimit)
		update      func(*LocalGovernanceStore, *configstoreTables.TableRateLimit)
		owner       func(*LocalGovernanceStore) *configstoreTables.TableRateLimit
		checkSlices func(*testing.T, *LocalGovernanceStore)
	}

	makeVK := func(id, secret string, rateLimit *configstoreTables.TableRateLimit, providerConfig bool) *configstoreTables.TableVirtualKey {
		vk := &configstoreTables.TableVirtualKey{
			ID:              "vk-" + id,
			Name:            "vk-" + id,
			Value:           *schemas.NewSecretVar(secret),
			CalendarAligned: true,
		}
		if providerConfig {
			vk.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{{
				ID: 1, Provider: "openai", RateLimitID: &id, RateLimit: rateLimit,
			}}
		} else {
			vk.RateLimitID = &id
			vk.RateLimit = rateLimit
		}
		return vk
	}

	makeRateLimit := func(id string, max int64) *configstoreTables.TableRateLimit {
		tokenDuration := "5m"
		requestDuration := "6m"
		calendarAlignedInput := true
		return &configstoreTables.TableRateLimit{
			ID: id, TokenMaxLimit: &max, TokenResetDuration: &tokenDuration,
			TokenLastReset: tokenReset, RequestMaxLimit: ptrInt64(max + 1),
			RequestResetDuration: &requestDuration, RequestLastReset: requestReset,
			CalendarAlignedInput: &calendarAlignedInput,
		}
	}

	var topVKInput *configstoreTables.TableVirtualKey
	var providerVKInput *configstoreTables.TableVirtualKey
	var modelOwner *configstoreTables.TableModelConfig
	cases := []publisherCase{
		{
			name: "virtual-key", id: "owner-vk", aligned: true,
			create: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				store.CreateVirtualKeyInMemory(ctx, makeVK("owner-vk", "sk-owner-vk", rateLimit, false))
			},
			update: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				topVKInput = makeVK("owner-vk", "sk-owner-vk", rateLimit, false)
				store.UpdateVirtualKeyInMemory(ctx, topVKInput, nil, nil, nil)
			},
			owner: func(store *LocalGovernanceStore) *configstoreTables.TableRateLimit {
				raw, _ := store.virtualKeys.Load("sk-owner-vk")
				return raw.(*configstoreTables.TableVirtualKey).RateLimit
			},
		},
		{
			name: "virtual-key-provider-config", id: "owner-vk-provider", aligned: true,
			create: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				store.CreateVirtualKeyInMemory(ctx, makeVK("owner-vk-provider", "sk-owner-vk-provider", rateLimit, true))
			},
			update: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				providerVKInput = makeVK("owner-vk-provider", "sk-owner-vk-provider", rateLimit, true)
				store.UpdateVirtualKeyInMemory(ctx, providerVKInput, nil, nil, nil)
			},
			owner: func(store *LocalGovernanceStore) *configstoreTables.TableRateLimit {
				raw, _ := store.virtualKeys.Load("sk-owner-vk-provider")
				return raw.(*configstoreTables.TableVirtualKey).ProviderConfigs[0].RateLimit
			},
			checkSlices: func(t *testing.T, store *LocalGovernanceStore) {
				stored, _ := store.virtualKeys.Load("sk-owner-vk-provider")
				storedVK := stored.(*configstoreTables.TableVirtualKey)
				assert.NotSame(t, &providerVKInput.ProviderConfigs[0], &storedVK.ProviderConfigs[0], "the provider-config slice must be detached from its caller")
			},
		},
		{
			name: "team", id: "owner-team", aligned: true,
			create: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				id := "owner-team"
				store.CreateTeamInMemory(ctx, &configstoreTables.TableTeam{ID: id, Name: id, RateLimitID: &id, RateLimit: rateLimit, CalendarAligned: true})
			},
			update: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				id := "owner-team"
				store.UpdateTeamInMemory(ctx, &configstoreTables.TableTeam{ID: id, Name: id, RateLimitID: &id, RateLimit: rateLimit, CalendarAligned: true}, nil)
			},
			owner: func(store *LocalGovernanceStore) *configstoreTables.TableRateLimit {
				team, _ := store.teams.Load("owner-team")
				return team.(*configstoreTables.TableTeam).RateLimit
			},
		},
		{
			name: "customer", id: "owner-customer", aligned: true,
			create: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				id := "owner-customer"
				store.CreateCustomerInMemory(ctx, &configstoreTables.TableCustomer{ID: id, Name: id, RateLimitID: &id, RateLimit: rateLimit, CalendarAligned: true})
			},
			update: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				id := "owner-customer"
				store.UpdateCustomerInMemory(ctx, &configstoreTables.TableCustomer{ID: id, Name: id, RateLimitID: &id, RateLimit: rateLimit, CalendarAligned: true}, nil)
			},
			owner: func(store *LocalGovernanceStore) *configstoreTables.TableRateLimit {
				customer, _ := store.customers.Load("owner-customer")
				return customer.(*configstoreTables.TableCustomer).RateLimit
			},
		},
		{
			name: "model-config", id: "owner-model-config", aligned: true,
			create: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				mc := buildModelConfig("owner-model-config", "owner-model", nil, nil, rateLimit)
				mc.CalendarAligned = true
				modelOwner = store.UpdateModelConfigInMemory(ctx, mc)
			},
			update: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				mc := buildModelConfig("owner-model-config", "owner-model", nil, nil, rateLimit)
				mc.CalendarAligned = true
				modelOwner = store.UpdateModelConfigInMemory(ctx, mc)
			},
			owner: func(*LocalGovernanceStore) *configstoreTables.TableRateLimit { return modelOwner.RateLimit },
		},
		{
			name: "provider", id: "owner-provider", aligned: false,
			create: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				store.UpdateProviderInMemory(ctx, buildProviderWithGovernance("owner-provider", nil, rateLimit))
			},
			update: func(store *LocalGovernanceStore, rateLimit *configstoreTables.TableRateLimit) {
				store.UpdateProviderInMemory(ctx, buildProviderWithGovernance("owner-provider", nil, rateLimit))
			},
			owner: func(store *LocalGovernanceStore) *configstoreTables.TableRateLimit {
				provider, _ := store.providers.Load("owner-provider")
				return provider.(*configstoreTables.TableProvider).RateLimit
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStandaloneStore(t)
			seed := makeRateLimit(tc.id, 100)
			tc.create(store, seed)
			createdMap := store.LoadRateLimit(ctx, tc.id)
			require.NotNil(t, createdMap)
			createdOwner := tc.owner(store)
			require.NotNil(t, createdOwner)
			assert.NotSame(t, seed, createdMap, "inserted config must be detached from its caller")
			assert.NotSame(t, seed, createdOwner, "inserted owner snapshot must be detached from its caller")
			assert.NotSame(t, createdMap, createdOwner, "owner snapshot must be detached from the authoritative map")
			assert.Equal(t, int64(100), *createdMap.TokenMaxLimit)
			assert.Equal(t, int64(101), *createdMap.RequestMaxLimit)
			assert.Equal(t, "5m", *createdMap.TokenResetDuration)
			assert.Equal(t, "6m", *createdMap.RequestResetDuration)
			assert.Equal(t, tc.aligned, createdMap.IsCalendarAligned)
			assert.False(t, seed.IsCalendarAligned, "owner alignment must not mutate the seed config")
			assert.NotSame(t, seed.TokenMaxLimit, createdMap.TokenMaxLimit)
			assert.NotSame(t, seed.TokenResetDuration, createdMap.TokenResetDuration)
			assert.NotSame(t, seed.RequestMaxLimit, createdMap.RequestMaxLimit)
			assert.NotSame(t, seed.RequestResetDuration, createdMap.RequestResetDuration)
			assert.NotSame(t, seed.CalendarAlignedInput, createdMap.CalendarAlignedInput)
			require.NoError(t, store.BumpRateLimitUsageBy(ctx, tc.id, 11, 4))
			previous := store.LoadRateLimit(ctx, tc.id)
			require.NotNil(t, previous)
			next := makeRateLimit(tc.id, 200)
			next.TokenCurrentUsage = 700
			next.RequestCurrentUsage = 800
			next.TokenLastReset = time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)
			next.RequestLastReset = time.Date(2026, time.April, 5, 6, 7, 8, 0, time.UTC)

			var hookAttempts atomic.Int64
			var armed atomic.Bool
			armed.Store(true)
			snapshotRead := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			store.rateLimitConfigPublishHook = func() {
				hookAttempts.Add(1)
				if armed.CompareAndSwap(true, false) {
					close(snapshotRead)
					<-release
				}
			}

			updateDone := make(chan struct{})
			go func() {
				defer close(updateDone)
				tc.update(store, next)
			}()
			<-snapshotRead
			require.NoError(t, store.BumpRateLimitUsageBy(ctx, tc.id, 7, 2))
			assert.NotSame(t, previous, store.LoadRateLimit(ctx, tc.id), "the usage writer must replace the snapshot before the config commit")
			releaseOnce.Do(func() { close(release) })
			<-updateDone

			authoritative := store.LoadRateLimit(ctx, tc.id)
			require.NotNil(t, authoritative)
			assert.Equal(t, int64(18), authoritative.TokenCurrentUsage)
			assert.Equal(t, int64(6), authoritative.RequestCurrentUsage)
			assert.True(t, authoritative.TokenLastReset.Equal(tokenReset))
			assert.True(t, authoritative.RequestLastReset.Equal(requestReset))
			assert.Equal(t, int64(200), *authoritative.TokenMaxLimit)
			assert.Equal(t, "5m", *authoritative.TokenResetDuration)
			assert.Equal(t, "6m", *authoritative.RequestResetDuration)
			assert.Equal(t, tc.aligned, authoritative.IsCalendarAligned)
			assert.GreaterOrEqual(t, hookAttempts.Load(), int64(2), "each owner publisher must retry after its stale snapshot loses the CAS")
			assert.Equal(t, int64(11), previous.TokenCurrentUsage, "the old map snapshot must remain immutable")
			assert.Equal(t, int64(4), previous.RequestCurrentUsage, "the old map snapshot must remain immutable")
			assert.True(t, previous.TokenLastReset.Equal(tokenReset))
			assert.True(t, previous.RequestLastReset.Equal(requestReset))

			owner := tc.owner(store)
			require.NotNil(t, owner)
			assert.NotSame(t, next, owner, "the owner snapshot must be detached from the caller")
			assert.NotSame(t, authoritative, owner, "the owner snapshot must be detached from the map entry")
			assert.NotSame(t, next.TokenMaxLimit, authoritative.TokenMaxLimit)
			assert.NotSame(t, next.TokenResetDuration, authoritative.TokenResetDuration)
			assert.NotSame(t, next.RequestMaxLimit, authoritative.RequestMaxLimit)
			assert.NotSame(t, next.RequestResetDuration, authoritative.RequestResetDuration)
			assert.NotSame(t, next.CalendarAlignedInput, authoritative.CalendarAlignedInput)
			assert.False(t, next.IsCalendarAligned, "derived alignment must not mutate the caller config")
			if tc.checkSlices != nil {
				tc.checkSlices(t, store)
			}

			*next.TokenMaxLimit = 300
			*owner.RequestMaxLimit = 400
			*next.CalendarAlignedInput = false
			assert.Equal(t, int64(200), *authoritative.TokenMaxLimit, "caller pointer mutation must not reach the map")
			assert.Equal(t, int64(201), *authoritative.RequestMaxLimit, "owner pointer mutation must not reach the map")
			assert.True(t, *authoritative.CalendarAlignedInput, "caller pointer mutation must not reach the map")
		})
	}
}

func TestLighthouseRateLimitAbsentInsertionCompetingPublisherPreservesUsage(t *testing.T) {
	ctx := context.Background()
	store := newStandaloneStore(t)
	rlID := "lighthouse-rate-limit-absent-race"
	first := buildRateLimit(rlID, 100, 100)
	second := buildRateLimit(rlID, 200, 200)
	var hookAttempts atomic.Int64
	var armed atomic.Bool
	armed.Store(true)
	snapshotRead := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	store.rateLimitConfigPublishHook = func() {
		hookAttempts.Add(1)
		if armed.CompareAndSwap(true, false) {
			close(snapshotRead)
			<-release
		}
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		store.UpsertRateLimitConfig(ctx, rlID, first)
	}()
	<-snapshotRead
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		store.UpsertRateLimitConfig(ctx, rlID, second)
	}()
	<-secondDone
	require.NoError(t, store.BumpRateLimitUsageBy(ctx, rlID, 9, 3))
	releaseOnce.Do(func() { close(release) })
	<-firstDone

	final := store.LoadRateLimit(ctx, rlID)
	require.NotNil(t, final)
	assert.Equal(t, int64(9), final.TokenCurrentUsage)
	assert.Equal(t, int64(3), final.RequestCurrentUsage)
	assert.Equal(t, int64(100), *final.TokenMaxLimit, "the retrying publisher should still publish its requested config")
	assert.GreaterOrEqual(t, hookAttempts.Load(), int64(3), "both insert attempts and the retry must be observed")
	assert.NotSame(t, first.TokenMaxLimit, final.TokenMaxLimit, "the caller config pointer must be detached")
}

func TestLighthouseRateLimitInvalidStoredValueUsesCAS(t *testing.T) {
	ctx := context.Background()
	store := newStandaloneStore(t)
	rlID := "lighthouse-rate-limit-invalid-value"
	invalid := "not-a-rate-limit"
	store.rateLimits.Store(rlID, invalid)
	config := buildRateLimit(rlID, 200, 200)
	var hookAttempts atomic.Int64
	var armed atomic.Bool
	armed.Store(true)
	snapshotRead := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	store.rateLimitConfigPublishHook = func() {
		hookAttempts.Add(1)
		if armed.CompareAndSwap(true, false) {
			close(snapshotRead)
			<-release
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		store.UpsertRateLimitConfig(ctx, rlID, config)
	}()
	<-snapshotRead
	replacement := buildRateLimitWithUsage(rlID, 300, 14, 400, 6)
	require.True(t, store.rateLimits.CompareAndSwap(rlID, invalid, replacement), "the competing publisher must replace the invalid entry")
	releaseOnce.Do(func() { close(release) })
	<-done

	final := store.LoadRateLimit(ctx, rlID)
	require.NotNil(t, final)
	assert.Equal(t, int64(14), final.TokenCurrentUsage)
	assert.Equal(t, int64(6), final.RequestCurrentUsage)
	assert.Equal(t, int64(200), *final.TokenMaxLimit)
	assert.GreaterOrEqual(t, hookAttempts.Load(), int64(2), "the invalid snapshot CAS must fail and retry")
}

// TestResetBudgetAt_ConcurrentResettersCollapse confirms that many goroutines
// all trying to reset the same budget to the same newLastReset deduplicate
// cleanly via CAS — exactly one resetter observes the transition, everyone
// else gets (nil, false). Without the re-check inside ResetBudgetAt, each
// goroutine would re-zero the counter and drop any increments applied in
// between.
func TestResetBudgetAt_ConcurrentResettersCollapse(t *testing.T) {
	store := newStandaloneStore(t)
	budgetID := "reset-collapse"
	const goroutines = 128
	// Exactly one window between the grant anchor and the reset target, so the
	// derived remaining count is unambiguous: 5 granted minus 1 window closed.
	newLastReset := time.Now().Truncate(time.Second)
	grantAnchor := newLastReset.Add(-time.Hour)

	old := buildBudget(budgetID, 1000, "1h")
	old.CreatedAt = grantAnchor
	old.LastReset = grantAnchor
	old.CurrentUsage = 999
	require.NoError(t, old.SetOverrideAt(25, configstoreTables.BudgetOverrideModeCycles, 5, grantAnchor))
	store.budgets.Store(budgetID, old)

	var successes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, ok := store.ResetBudgetAt(context.Background(), budgetID, newLastReset); ok {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), successes.Load(), "exactly one resetter should win the CAS when all target the same newLastReset")
	final := store.LoadBudget(context.Background(), budgetID)
	require.NotNil(t, final)
	assert.Equal(t, 0.0, final.CurrentUsage)
	assert.True(t, final.LastReset.Equal(newLastReset))
	assert.Equal(t, 4, final.OverrideCyclesRemaining, "the single winning reset should consume exactly one override cycle")
}

// TestAdoptCalendarAlignmentInMemoryPreservesConcurrentSpend pins that adopting a
// budget onto the calendar grid keeps whatever usage landed while the switch was
// in flight.
//
// Adoption cannot read usage, then write it back: a request bumping the same
// budget between those two steps would have its spend silently dropped. The CAS
// loop has to carry the usage it observed at swap time, which is what this test
// forces by bumping usage from another goroutine during the adoption.
func TestAdoptCalendarAlignmentInMemoryPreservesConcurrentSpend(t *testing.T) {
	ctx := context.Background()
	store := newStandaloneStore(t)
	now := time.Date(2026, time.February, 5, 12, 0, 0, 0, time.UTC)
	monthStart := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)

	budget := &configstoreTables.TableBudget{
		ID:                "adopt-live-budget",
		MaxLimit:          1000,
		CurrentUsage:      0,
		ResetDuration:     "1M",
		IsCalendarAligned: true,
		CreatedAt:         time.Date(2026, time.January, 10, 9, 0, 0, 0, time.UTC),
		LastReset:         time.Date(2026, time.January, 10, 9, 0, 0, 0, time.UTC),
	}
	store.budgets.Store(budget.ID, budget)

	// Usage is bumped by direct CAS rather than through BumpBudgetUsage, which
	// consults the real clock: a January window is long overdue against it, so the
	// request path would reset the budget onto the current real month and the fixed
	// `now` below could no longer move it. The property under test is that the
	// adoption CAS carries whatever usage it observed, and a plain increment races
	// it just as well without dragging real time into the fixture.
	const bumps = 50
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < bumps; i++ {
			for {
				raw, ok := store.budgets.Load(budget.ID)
				require.True(t, ok)
				current := raw.(*configstoreTables.TableBudget)
				clone := *current
				clone.CurrentUsage++
				if store.budgets.CompareAndSwap(budget.ID, raw, &clone) {
					break
				}
			}
		}
	}()
	adopted := store.AdoptCalendarAlignmentInMemory(ctx, budget.ID, now)
	wg.Wait()

	assert.True(t, adopted, "a window opened before the boundary must be adopted")

	live := store.LoadBudget(ctx, budget.ID)
	require.NotNil(t, live)
	assert.True(t, live.LastReset.Equal(monthStart), "the window is re-anchored on the boundary")
	assert.Equal(t, float64(bumps), live.CurrentUsage,
		"every concurrent bump survived: adoption changed the boundary, not the accounting")
}
