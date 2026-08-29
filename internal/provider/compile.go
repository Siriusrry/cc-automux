package provider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/Siriusrry/cc-automux/internal/config"
)

// CompiledProvider is the immutable, runtime-ready form of one configured
// provider. It contains only static data; health and scheduling state are not
// stored here.
type CompiledProvider struct {
	ID            string
	Name          string
	BaseURL       *url.URL
	APIKey        string
	Models        []string
	Priority      int64
	Enabled       bool
	UseXAPIKey    bool
	TLS           *tls.Config
	TLSSettings   config.TLSConfig
	Patches       []PatchMetadata
	DisableHealth bool
	Generation    ProviderGeneration

	modelSet map[string]struct{}
}

// Compile validates and compiles one provider using the default preset
// registry. Known but unimplemented presets are retained as metadata so the
// configuration entry point can round-trip them; ApplyPatches fails closed.
func Compile(input config.ProviderConfig) (*CompiledProvider, error) {
	return CompileWithRegistry(input, DefaultRegistry())
}

func CompileWithRegistry(input config.ProviderConfig, registry Registry) (*CompiledProvider, error) {
	if registry.Empty() {
		registry = DefaultRegistry()
	}
	// The config package owns the schema-level provider rules. Wrapping this one
	// provider in a complete configuration keeps the two packages from duplicating
	// validation logic while still allowing the provider compiler to be used on
	// its own.
	check := config.Default()
	check.Auth.ManagementKey = "provider-compiler-validation-key"
	check.Providers = []config.ProviderConfig{input}
	if err := check.Validate(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(input.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("base_url: %w", err)
	}
	tlsConfig, err := compileTLS(input.TLS)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	if err := registry.Validate(input.Patches); err != nil {
		return nil, err
	}
	patches := make([]PatchMetadata, 0, len(input.Patches))
	for _, id := range input.Patches {
		entry, _ := registry.Lookup(id)
		patches = append(patches, entry)
	}
	models := append([]string(nil), input.Models...)
	modelSet := make(map[string]struct{}, len(models))
	for _, model := range models {
		modelSet[model] = struct{}{}
	}
	return &CompiledProvider{
		ID:            input.ID,
		Name:          input.Name,
		BaseURL:       parsed,
		APIKey:        input.APIKey,
		Models:        models,
		Priority:      input.Priority,
		Enabled:       input.Enabled,
		UseXAPIKey:    input.UseXAPIKey,
		TLS:           tlsConfig,
		TLSSettings:   input.TLS,
		Patches:       patches,
		DisableHealth: input.DisableHealth,
		Generation:    generationFor(input),
		modelSet:      modelSet,
	}, nil
}

// CompileCatalog compiles every provider and builds an exact, case-sensitive
// model index. The returned catalog owns all slices and maps.
func CompileCatalog(inputs []config.ProviderConfig) (*Catalog, error) {
	return CompileCatalogWithRegistry(inputs, DefaultRegistry())
}

func CompileCatalogWithRegistry(inputs []config.ProviderConfig, registry Registry) (*Catalog, error) {
	if registry.Empty() {
		registry = DefaultRegistry()
	}
	// Validate root-level duplicate IDs/names and auth-independent schema rules
	// before compiling. A complete config also catches duplicate models.
	check := config.Default()
	check.Auth.ManagementKey = "provider-catalog-validation-key"
	check.Providers = append([]config.ProviderConfig(nil), inputs...)
	if err := check.Validate(); err != nil {
		return nil, err
	}
	providers := make([]*CompiledProvider, 0, len(inputs))
	index := make(map[string][]*CompiledProvider)
	for _, input := range inputs {
		compiled, err := CompileWithRegistry(input, registry)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", input.ID, err)
		}
		providers = append(providers, compiled)
		if !compiled.Enabled || len(compiled.Models) == 0 {
			continue
		}
		for _, model := range compiled.Models {
			index[model] = append(index[model], compiled)
		}
	}
	for model, candidates := range index {
		stablePrioritySort(candidates)
		index[model] = candidates
	}
	return &Catalog{providers: providers, index: index}, nil
}

func stablePrioritySort(items []*CompiledProvider) {
	for i := 1; i < len(items); i++ {
		current := items[i]
		j := i
		for j > 0 && items[j-1].Priority < current.Priority {
			items[j] = items[j-1]
			j--
		}
		items[j] = current
	}
}

func compileTLS(input config.TLSConfig) (*tls.Config, error) {
	if input.CAFile != "" && input.InsecureSkipVerify {
		return nil, errors.New("ca_file and insecure_skip_verify are mutually exclusive")
	}
	if input.CAFile == "" {
		if input.InsecureSkipVerify {
			return &tls.Config{InsecureSkipVerify: true}, nil // explicitly requested by config
		}
		// nil delegates to the Go transport's system certificate pool.
		return nil, nil
	}
	pemBytes, err := os.ReadFile(input.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("CA file contains no certificates")
	}
	return &tls.Config{RootCAs: pool}, nil
}

// ValidateApplication returns an error if a configured preset cannot actually
// be executed. It is intentionally separate from Compile: known
// presets may be stored and displayed, but no request path may silently ignore
// them.
func (p *CompiledProvider) ValidateApplication() error {
	for _, patch := range p.Patches {
		if !patch.Implemented {
			return fmt.Errorf("%w: %q", ErrPatchNotImplemented, patch.ID)
		}
	}
	return nil
}

// ApplyPatches is the explicit fail-closed hook for request processing. Any
// configured preset without an implementation returns an error rather than
// being silently ignored.
func (p *CompiledProvider) ApplyPatches() error {
	return p.ValidateApplication()
}

// Clone returns a defensive copy suitable for an API or snapshot boundary.
func (p *CompiledProvider) Clone() *CompiledProvider {
	if p == nil {
		return nil
	}
	out := *p
	out.Models = append([]string(nil), p.Models...)
	out.Patches = append([]PatchMetadata(nil), p.Patches...)
	if p.BaseURL != nil {
		urlCopy := *p.BaseURL
		out.BaseURL = &urlCopy
	}
	if p.TLS != nil {
		out.TLS = p.TLS.Clone()
	}
	// TLSSettings contains only strings and a bool, so the value copy is enough.
	out.modelSet = make(map[string]struct{}, len(p.modelSet))
	for model := range p.modelSet {
		out.modelSet[model] = struct{}{}
	}
	return &out
}

func (p *CompiledProvider) SupportsModel(model string) bool {
	if p == nil || !p.Enabled {
		return false
	}
	_, ok := p.modelSet[model]
	return ok
}

func (p *CompiledProvider) URLString() string {
	if p == nil || p.BaseURL == nil {
		return ""
	}
	return p.BaseURL.String()
}

// TLSClone returns a client-safe copy. A nil result means system roots.
func (p *CompiledProvider) TLSClone() *tls.Config {
	if p == nil || p.TLS == nil {
		return nil
	}
	return p.TLS.Clone()
}

// Config returns a defensive config representation of the compiled provider.
func (p *CompiledProvider) Config() config.ProviderConfig {
	if p == nil {
		return config.ProviderConfig{}
	}
	out := config.ProviderConfig{
		ID:            p.ID,
		Name:          p.Name,
		BaseURL:       p.URLString(),
		APIKey:        p.APIKey,
		Models:        append([]string(nil), p.Models...),
		Priority:      p.Priority,
		Enabled:       p.Enabled,
		UseXAPIKey:    p.UseXAPIKey,
		TLS:           p.TLSSettings,
		Patches:       make([]string, len(p.Patches)),
		DisableHealth: p.DisableHealth,
	}
	for i, patch := range p.Patches {
		out.Patches[i] = patch.ID
	}
	return out
}

func (p *CompiledProvider) String() string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("provider{id=%s,name=%s,url=%s}", p.ID, p.Name, p.URLString())
}
