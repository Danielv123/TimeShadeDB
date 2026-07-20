package timeshadedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	baselineDatastoreVersion = 1
	dataManifestFormat       = "timeShadeDB"
	rootManifestFormat       = "timeShadeDB-root"
	generationsDir           = "generations"
	currentDatastoreVersion  = baselineDatastoreVersion + len(datastoreMigrations)
)

type datastoreMigration struct {
	name string
	run  func(string) error
}

var datastoreMigrations = [...]datastoreMigration{}

type rootManifest struct {
	Format     string `json:"format"`
	Generation string `json:"generation"`
}

func openExistingDBWithMigrations(opts OpenOptions, migrations []datastoreMigration) (*DB, error) {
	if err := validateMigrations(migrations); err != nil {
		return nil, err
	}
	current := baselineDatastoreVersion + len(migrations)
	dataPath, m, err := resolveDatastore(opts.Path)
	if err != nil {
		return nil, err
	}
	version := m.DatastoreVersion
	if version > current {
		return nil, fmt.Errorf("timeshadedb: datastore version %d is newer than supported version %d", version, current)
	}
	if version < current {
		if opts.ReadOnly {
			return nil, fmt.Errorf("timeshadedb: datastore version %d requires migration to version %d", version, current)
		}
		return migrateDatastore(opts, dataPath, m, migrations)
	}

	opts.Path = dataPath
	return loadDBWithManifest(opts, m)
}

func validateMigrations(migrations []datastoreMigration) error {
	for i, migration := range migrations {
		if migration.name == "" || migration.run == nil {
			return fmt.Errorf("timeshadedb: invalid datastore migration from version %d", baselineDatastoreVersion+i)
		}
	}
	return nil
}

func resolveDatastore(root string) (string, manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return "", manifest{}, err
	}
	var envelope struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", manifest{}, err
	}
	if envelope.Format == dataManifestFormat {
		m, err := decodeDataManifest(data)
		if err == nil {
			err = validateManifest(root, m)
		}
		return root, m, err
	}
	if envelope.Format != rootManifestFormat {
		return "", manifest{}, fmt.Errorf("timeshadedb: unsupported manifest in %s", root)
	}
	var selector rootManifest
	if err := json.Unmarshal(data, &selector); err != nil {
		return "", manifest{}, err
	}
	if !validGenerationName(selector.Generation) {
		return "", manifest{}, fmt.Errorf("timeshadedb: invalid generation %q", selector.Generation)
	}
	path := filepath.Join(root, generationsDir, selector.Generation)
	m, err := readManifest(path)
	return path, m, err
}

func validGenerationName(name string) bool {
	if name == "" || filepath.Base(name) != name {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return name != "." && name != ".."
}

func migrateDatastore(opts OpenOptions, source string, m manifest, migrations []datastoreMigration) (*DB, error) {
	root := opts.Path
	current := baselineDatastoreVersion + len(migrations)
	generations := filepath.Join(root, generationsDir)
	if err := os.MkdirAll(generations, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(generations, ".staging-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	sourceInfo, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	flatSource := filepath.Clean(source) == filepath.Clean(root)
	if err := copyDatastore(source, staging, flatSource); err != nil {
		return nil, err
	}
	if err := os.Chmod(staging, sourceInfo.Mode().Perm()); err != nil {
		return nil, err
	}
	if m.StorageVersion == 1 {
		needsNormalization, err := legacyNeedsNormalization(staging)
		if err != nil {
			return nil, err
		}
		if needsNormalization {
			if err := normalizeStaging(staging); err != nil {
				return nil, err
			}
		}
	}

	for fromVersion := m.DatastoreVersion; fromVersion < current; fromVersion++ {
		migration := migrations[fromVersion-baselineDatastoreVersion]
		if err := migration.run(staging); err != nil {
			return nil, fmt.Errorf("timeshadedb: migration %q: %w", migration.name, err)
		}
		m, err = readDataManifest(staging)
		if err != nil {
			return nil, err
		}
		m.DatastoreVersion = fromVersion + 1
		if err := writeDataManifest(staging, m); err != nil {
			return nil, err
		}
	}

	suffix := strings.TrimPrefix(filepath.Base(staging), ".staging-")
	name := fmt.Sprintf("v%06d-%s", m.DatastoreVersion, suffix)
	generationPath := filepath.Join(generations, name)
	if err := replaceFileAtomically(staging, generationPath); err != nil {
		return nil, err
	}
	opts.Path = generationPath
	db, err := loadDB(opts)
	if err != nil {
		_ = os.RemoveAll(generationPath)
		return nil, err
	}
	if err := writeRootManifest(root, name); err != nil {
		_ = db.Close()
		_ = os.RemoveAll(generationPath)
		return nil, err
	}
	return db, nil
}

func legacyNeedsNormalization(path string) (bool, error) {
	for y := 0; y < TileRows; y++ {
		for x := 0; x < TileCols; x++ {
			info, err := os.Stat(walPath(path, x, y))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return false, err
			}
			if info.Size() != 0 && info.Size() != 6 {
				return true, nil
			}
		}
	}
	return false, nil
}

func normalizeStaging(path string) error {
	db, err := loadDB(OpenOptions{Path: path})
	if err != nil {
		return err
	}
	return db.Close()
}

func copyDatastore(source, destination string, excludeGenerations bool) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	source = resolved
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if excludeGenerations && rel == generationsDir && entry.IsDir() {
			return filepath.SkipDir
		}
		if !entry.IsDir() && isMigrationScratch(entry.Name()) {
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("timeshadedb: cannot migrate non-regular file %s", path)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func isMigrationScratch(name string) bool {
	return strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".compact") || strings.HasSuffix(name, ".bak")
}

func copyFile(source, destination string, mode fs.FileMode) (retErr error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, out.Close())
	}()
	if err := out.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func baseDataManifest(storageVersion int) manifest {
	return manifest{
		Format:           dataManifestFormat,
		StorageVersion:   storageVersion,
		DatastoreVersion: currentDatastoreVersion,
		Codec:            "zstd",
		CodecLevel:       9,
	}
}

func readDataManifest(path string) (manifest, error) {
	data, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		return manifest{}, err
	}
	return decodeDataManifest(data)
}

func decodeDataManifest(data []byte) (manifest, error) {
	m := manifest{DatastoreVersion: baselineDatastoreVersion}
	err := json.Unmarshal(data, &m)
	return m, err
}

func writeDataManifest(path string, m manifest) error {
	data, err := marshalJSON(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "manifest.json"), data, 0o644)
}

func writeRootManifest(path, generation string) error {
	return writeJSONAtomically(filepath.Join(path, "manifest.json"), rootManifest{
		Format:     rootManifestFormat,
		Generation: generation,
	})
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	return append(data, '\n'), err
}

func writeJSONAtomically(path string, value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFileAtomically(tmpPath, path)
}
