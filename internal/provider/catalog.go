package provider

// Catalog is an immutable static provider catalog and exact model index.
// Scheduling, health, sticky sessions, and failure state deliberately do not
// belong here.
type Catalog struct {
	providers []*CompiledProvider
	index     map[string][]*CompiledProvider
}

func (c *Catalog) Clone() *Catalog {
	if c == nil {
		return &Catalog{providers: []*CompiledProvider{}, index: map[string][]*CompiledProvider{}}
	}
	out := &Catalog{
		providers: make([]*CompiledProvider, len(c.providers)),
		index:     make(map[string][]*CompiledProvider, len(c.index)),
	}
	byID := make(map[string]*CompiledProvider, len(c.providers))
	for i, item := range c.providers {
		clone := item.Clone()
		out.providers[i] = clone
		byID[clone.ID] = clone
	}
	for model, items := range c.index {
		clones := make([]*CompiledProvider, len(items))
		for i, item := range items {
			clones[i] = byID[item.ID]
		}
		out.index[model] = clones
	}
	return out
}

func (c *Catalog) Providers() []*CompiledProvider {
	if c == nil {
		return []*CompiledProvider{}
	}
	result := make([]*CompiledProvider, len(c.providers))
	for i, p := range c.providers {
		result[i] = p.Clone()
	}
	return result
}

// Match returns enabled providers declaring the exact model string, ordered by
// descending priority and then original configuration order.
func (c *Catalog) Match(model string) []*CompiledProvider {
	if c == nil {
		return []*CompiledProvider{}
	}
	items := c.index[model]
	result := make([]*CompiledProvider, len(items))
	for i, p := range items {
		result[i] = p.Clone()
	}
	return result
}

func (c *Catalog) HasModel(model string) bool {
	return c != nil && len(c.index[model]) > 0
}

func (c *Catalog) ModelNames() []string {
	if c == nil {
		return []string{}
	}
	result := make([]string, 0, len(c.index))
	for model := range c.index {
		result = append(result, model)
	}
	// deterministic insertion sort, avoiding another dependency
	for i := 1; i < len(result); i++ {
		v := result[i]
		j := i
		for j > 0 && result[j-1] > v {
			result[j] = result[j-1]
			j--
		}
		result[j] = v
	}
	return result
}
