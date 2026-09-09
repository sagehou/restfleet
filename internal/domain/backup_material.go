package domain

// BackupMaterial is CENTRAL-ONLY metadata plus encrypted cloud configuration.
// It contains no repository password, master key or Agent session capability.
type BackupMaterial struct {
	Admission  BackupAdmission
	Credential StorageCredential
	Envelope   SecretEnvelope
}
