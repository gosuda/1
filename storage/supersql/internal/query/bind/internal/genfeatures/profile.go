package main

import "fmt"

func validateFeature(entry feature) error {
	disposition := "unsupported"
	state := "0A000"
	switch entry.ID {
	case "server_transport", "cluster_admin", "authorization", "external_code",
		"foreign_data", "replication", "prepared_transactions", "weaker_isolation",
		"notifications", "server_io", "xml", "locale_providers", "physical_storage",
		"non_btree_indexes", "advanced_exclusion", "rewrite_hooks", "physical_maintenance",
		"server_observability", "aggregate_or_replace", "session_state",
		"write_descriptor_expansion", "datetime_fields", "encoding_conversion":
	case "required_sql":
		disposition, state = "required", ""
	case "psql_commands":
		disposition, state = "client_command", ""
	default:
		return fmt.Errorf("%w: unknown feature %s", errInvalidManifest, entry.ID)
	}
	if entry.Disposition != disposition || entry.SQLState != state {
		return fmt.Errorf("%w: changed disposition for %s", errInvalidManifest, entry.ID)
	}
	return nil
}
