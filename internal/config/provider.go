package config

// SecretRole describes one provider-defined credential value without ever carrying that value.
type SecretRole struct {
	Name        string
	Description string
}

// TargetMetadata describes the non-secret target bound to a connection.
type TargetMetadata struct {
	Label       string
	Description string
	Required    bool
}

// ProviderMetadata is the configuration contract of one compiled provider.
type ProviderMetadata struct {
	ID             string
	Name           string
	DefaultBaseURL string
	SecretRoles    []SecretRole
	Target         TargetMetadata
}

// ProviderCatalog is the provider metadata view used by configuration and user interfaces.
type ProviderCatalog interface {
	ProviderMetadata(string) (ProviderMetadata, bool)
	ProviderMetadataAll() []ProviderMetadata
}

type emptyProviderCatalog struct{}

func (emptyProviderCatalog) ProviderMetadata(string) (ProviderMetadata, bool) {
	return ProviderMetadata{}, false
}
func (emptyProviderCatalog) ProviderMetadataAll() []ProviderMetadata { return nil }
