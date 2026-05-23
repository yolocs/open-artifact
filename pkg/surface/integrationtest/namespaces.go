package integrationtest

import (
	"context"

	"github.com/yolocs/open-artifact/pkg/auth"
	"github.com/yolocs/open-artifact/pkg/core"
	"github.com/yolocs/open-artifact/pkg/namespace"
)

func SeedNamespace(ctx context.Context, registry *namespace.Registry, spec namespace.Spec, store core.Store) error {
	if err := registry.Put(ctx, spec); err != nil {
		return err
	}
	if store != nil {
		return registry.PutStore(ctx, spec.Name, store)
	}
	return nil
}

func HostedAnonymous(name string) namespace.Spec {
	anon := anonymousMatcher()
	return namespace.Spec{
		Name: name,
		Kind: namespace.KindHosted,
		Policy: namespace.Policy{
			Readers: []namespace.SubjectMatcher{anon},
			Writers: []namespace.SubjectMatcher{anon},
		},
	}
}

func ProxyAnonymous(name, upstreamURL string) namespace.Spec {
	return namespace.Spec{
		Name:        name,
		Kind:        namespace.KindProxy,
		UpstreamURL: upstreamURL,
		Policy: namespace.Policy{
			Readers: []namespace.SubjectMatcher{anonymousMatcher()},
		},
	}
}

func DenyAll(name string) namespace.Spec {
	return namespace.Spec{Name: name, Kind: namespace.KindHosted}
}

func ReadOnlyAnonymous(name string) namespace.Spec {
	return namespace.Spec{
		Name: name,
		Kind: namespace.KindHosted,
		Policy: namespace.Policy{
			Readers: []namespace.SubjectMatcher{anonymousMatcher()},
		},
	}
}

func anonymousMatcher() namespace.SubjectMatcher {
	return namespace.SubjectMatcher{
		Issuer: auth.AnonymousIssuer,
		ID:     auth.AnonymousID,
		Kind:   auth.KindAnonymous,
	}
}
