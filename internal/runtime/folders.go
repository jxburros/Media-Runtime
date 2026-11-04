package runtime

// RefreshFolders rescans approved locations in place. Missing files remain registered with
// their original identity so unplugging a drive never becomes a destructive catalog update.
func (r *Runtime) RefreshFolders() error {
	entries, err := r.Folders.Scan()
	if err != nil {
		return err
	}
	r.Config.AddArtifacts(entries)
	for _, member := range r.Pool.Members {
		if err := member.Client.RefreshCatalog(); err != nil {
			return err
		}
	}
	return nil
}
