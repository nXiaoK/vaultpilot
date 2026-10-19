package platform

const (
	backendOneDrive    = "onedrive"
	backendGoogleDrive = "gdrive"
)

func cloudBackend(backend string) bool {
	return backend == backendOneDrive || backend == backendGoogleDrive
}

func backendLabel(backend string) string {
	if backend == backendGoogleDrive {
		return "Google Drive"
	}
	return "OneDrive"
}

func rcloneType(backend string) string {
	if backend == backendGoogleDrive {
		return "drive"
	}
	return "onedrive"
}
