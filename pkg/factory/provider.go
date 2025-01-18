package factory

import "context"

type Provider = any

// NewProviderFunc is a function that creates a new Provider.
type NewProviderFunc[P Provider, C Config] func(context.Context, ProviderSettings, C) (P, error)

type ProviderFactory[P Provider, C Config] interface {
	Named
	New(context.Context, ProviderSettings, C) (P, error)
}

type providerFactory[P Provider, C Config] struct {
	name            Name
	newProviderFunc NewProviderFunc[P, C]
}

func (factory *providerFactory[P, C]) Name() Name {
	return factory.name
}

func (factory *providerFactory[P, C]) New(ctx context.Context, settings ProviderSettings, config C) (P, error) {
	return factory.newProviderFunc(ctx, settings, config)
}

func NewProviderFactory[P Provider, C Config](name Name, newProviderFunc NewProviderFunc[P, C]) ProviderFactory[P, C] {
	return &providerFactory[P, C]{
		name:            name,
		newProviderFunc: newProviderFunc,
	}
}

func NewFromFactory[P Provider, C Config](ctx context.Context, settings ProviderSettings, config C, factories NamedMap[ProviderFactory[P, C]], key string) (p P, err error) {
	providerFactory, err := factories.Get(key)
	if err != nil {
		return
	}

	provider, err := providerFactory.New(ctx, settings, config)
	if err != nil {
		return
	}

	p = provider
	return
}