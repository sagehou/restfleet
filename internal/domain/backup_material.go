package domain

// BackupMaterial is CENTRAL-ONLY metadata plus encrypted cloud configuration.
// It contains no repository password, master key or Agent session capability.
type BackupMaterial struct {
	Admission  BackupAdmission
	Credential StorageCredential
	Envelope   SecretEnvelope
}

// GatewayDeliveryMaterial is CENTRAL-ONLY; the database envelope is opened only
// centrally before sealing a separate Gateway payload. The origin and latest
// authorization were checked together
// with current identity/ACK and the initial cloud configuration revision.
type GatewayDeliveryMaterial struct {
	Material BackupMaterial
	Origin   GatewayPendingOrigin
	Decision GatewayDecision
}
