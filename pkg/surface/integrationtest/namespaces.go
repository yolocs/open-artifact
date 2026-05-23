package integrationtest

import (
	"context"

	"github.com/yolocs/open-artifact/pkg/namespace"
)

func SeedNamespace(ctx context.Context, store *namespace.Store, ns *namespace.Namespace) error {
	_, err := store.Put(ctx, ns)
	return err
}

func HostedAnonymous(name string) *namespace.Namespace {
	anon := anonymousMatcher()
	return &namespace.Namespace{
		Name: name,
		Spec: namespace.Spec{
			Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{anon},
				Writers: []namespace.SubjectMatcher{anon},
			},
		},
	}
}

func ProxyAnonymous(name, upstreamURL string) *namespace.Namespace {
	return &namespace.Namespace{
		Name: name,
		Spec: namespace.Spec{
			Mode: namespace.ModeProxy,
			Proxy: namespace.Proxy{
				Upstream: upstreamURL,
			},
			Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{anonymousMatcher()},
			},
		},
	}
}

func DenyAll(name string) *namespace.Namespace {
	return &namespace.Namespace{Name: name, Spec: namespace.Spec{}}
}

func ReadOnlyAnonymous(name string) *namespace.Namespace {
	return &namespace.Namespace{
		Name: name,
		Spec: namespace.Spec{
			Policy: namespace.Policy{
				Readers: []namespace.SubjectMatcher{anonymousMatcher()},
			},
		},
	}
}

func anonymousMatcher() namespace.SubjectMatcher {
	return namespace.SubjectMatcher{
		Issuer:   "anonymous",
		SubMatch: "anonymous",
		Kind:     namespace.KindAnonymous,
	}
}
