// Package torbox provides an interface to the Torbox.app
// object storage system.
package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

// Constants
const (
	minSleep      = 10 * time.Millisecond
	maxSleep      = 2 * time.Second
	decayConstant = 2 // bigger for slower decay, exponential
	rootID        = "0" // ID of root folder - need to confirm with Torbox API
	rootURL       = "https://api.torbox.app/v1/api" // Base URL for Torbox API
	cacheExpiry   = time.Minute
)

// Globals
var (
// No OAuth for Torbox based on the provided API spec, using API key instead
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "torbox",
		Description: "Torbox.app",
		NewFs:       NewFs,
		// Config: func(ctx context.Context, name string, m configmap.Mapper, config fs.ConfigIn) (*fs.ConfigOut, error) {
		// 	return &fs.ConfigOut{}, nil // No OAuth, so return empty ConfigOut
		// },
		Options: []fs.Option{{
			Name: "api_key",
			Help: `API Key for Torbox.app.
This is required and must be created in your Torbox account.`,
			// Hide:      fs.OptionHideBoth,
			Required:  true,
			Sensitive: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			// Encode invalid UTF-8 bytes as json doesn't handle them properly.
			Default: (encoder.Display |
				encoder.EncodeBackSlash |
				encoder.EncodeDoubleQuote |
				encoder.EncodeInvalidUtf8),
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	APIKey string               `config:"api_key"`
	Enc    encoder.MultiEncoder `config:"encoding"`
}

// Define the file struct type at the package level
type TorboxFile struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
	Zipped   bool   `json:"zipped"`
	S3Path   string `json:"s3_path"`
	Infected bool   `json:"infected"`
	Mimetype string `json:"mimetype"`
}

// Define the torrent struct type at the package level
type TorboxTorrent struct {
	ID    int          `json:"id"`
	Name  string       `json:"name"`
	Hash  string       `json:"hash,omitempty"`
	Size  int64        `json:"size"`
	Type  string       `json:"type"`
	Files []TorboxFile `json:"files"`
}

// Fs represents a remote cloud storage system
type Fs struct {
	name         string             // name of this remote
	root         string             // the path we are working on
	opt          Options            // parsed options
	features     *fs.Features       // optional features
	srv          *rest.Client       // the connection to the server
	dirCache     *dircache.DirCache // Map of directory path to directory id
	pacer        *fs.Pacer          // pacer for API calls
	// Cache for items (torrents, usenet, web downloads)
	cache struct {
		items      []TorboxTorrent
		lastUpdated time.Time
		mu          sync.Mutex // Mutex to protect the cache
	}
}

// Precision returns the precision of the backend
func (f *Fs) Precision() time.Duration {
	// Need to confirm precision with Torbox API
	return fs.ModTimeNotSupported
}

// CreateDir makes a directory with path dir
func (f *Fs) CreateDir(ctx context.Context, dir string, pathID string) (newDirID string, err error) {
	// The Torbox API does not have a dedicated endpoint for creating directories.
	// Directories are implicitly created when uploading files with paths.
	// Returning fs.ErrorCantMkdir as per the Rmdir implementation.
	// return "", fs.ErrorCantMkdir
	return "", fmt.Errorf("CreateDir: %w", err)
}

// FindLeaf finds a directory of name leaf in the directory dir.
// It returns the directory ID of the leaf, and error = fs.ErrorDirNotFound
// if it can't be found.
func (f *Fs) FindLeaf(ctx context.Context, dir string, leaf string) (pathID string, found bool, err error) {
	// Need to implement finding a directory based on Torbox API.
	// Since the API provides a flat list of items, we don't have a hierarchical structure.
	// For now, returning fs.ErrorDirNotFound as we treat everything at the root.
	return "", false, fs.ErrorDirNotFound
}

// Object describes a file
type Object struct {
	fs          *Fs       // what this object is part of
	remote      string    // The remote path
	hasMetaData bool      // metadata is present and correct
	size        int64     // size of the object
	modTime     time.Time // modification time of the object
	id          string    // ID of the object - need to confirm with Torbox API
	parentID    string    // ID of parent directory - need to confirm with Torbox API
	mimeType    string    // Mime type of object
	url         string    // URL to download file
	// Additional metadata from torrent files
	fileID      int       // File ID within the torrent
	hash        string    // Hash of the torrent this file belongs to
	s3Path      string    // S3 path of the file
	infected    bool      // Whether file is infected
	zipped      bool      // Whether file is zipped
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("torbox.app root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// parsePath parses a torbox.app 'url'
func parsePath(path string) (root string) {
	root = strings.Trim(path, "/")
	return
}

// retryErrorCodes is a slice of error codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
}

// shouldRetry returns a boolean as to whether this resp and err
// deserve to be retried.  It returns the err as a convenience
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	fs.Debugf(nil, "torbox: Handling error response with status %d (%s)", resp.StatusCode, resp.Status)
	
	body, err := rest.ReadBody(resp)
	if err != nil {
		fs.Debugf(nil, "torbox: Failed to read error response body: %v", err)
		body = nil
	}
	var e struct {
		Success bool `json:"success"`
		Error   string `json:"error"`
		Detail  string `json:"detail"`
	}
	if body != nil {
		// Try to unmarshal the error response
		if json.Unmarshal(body, &e) == nil && !e.Success {
			fs.Debugf(nil, "torbox: Parsed API error - Error: %q, Detail: %q", e.Error, e.Detail)
			return errors.New(e.Error + ": " + e.Detail)
		}
		fs.Debugf(nil, "torbox: Failed to parse error response as JSON, body: %s", string(body))
	}
	// If unmarshalling fails or it's not a standard error response,
	// fall back to the default error message
	fs.Debugf(nil, "torbox: Using default error message for HTTP %d", resp.StatusCode)
	return fmt.Errorf("HTTP error: %s (%d)", resp.Status, resp.StatusCode)
}

// Return a url.Values without the api key since it will be in header
func (f *Fs) baseParams() url.Values {
	return url.Values{} // No longer need to add API key here
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	root = parsePath(root)
	fs.Debugf(nil, "torbox: Creating new filesystem with name=%q, root=%q", name, root)

	client := fshttp.NewClient(ctx)

	f := &Fs{
		name:  name,
		root:  root,
		opt:   *opt,
		srv:   rest.NewClient(client).SetRoot(rootURL),
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decayConstant))),
	}
	f.features = (&fs.Features{
		CaseInsensitive:         false, // Need to confirm with Torbox API
		CanHaveEmptyDirectories: false, // Need to confirm with Torbox API
		ReadMimeType:            false, // Need to confirm with Torbox API
	}).Fill(ctx, f)
	f.srv.SetErrorHandler(errorHandler)

	// Get rootID - Need to determine how to get root ID from Torbox API
	// For now, assuming a root ID like "0" or similar if the API supports it.
	// If not, we might need a different approach for the root.
	f.dirCache = dircache.New(root, rootID, f)

	fs.Debugf(f, "torbox: Filesystem created successfully")
	return f, nil
}

// Implement other fs interface methods here based on Torbox API
// List, NewObject, Put, Mkdir, Rmdir, Purge, Move, DirMove, PublicLink, Shutdown, About, etc.

// List the objects and directories in dir into entries.
// This should return ErrDirNotFound if the directory isn't found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "Listing directory %q", dir)
	
	if err := f.ensureFreshCache(ctx); err != nil {
		return nil, fmt.Errorf("failed to refresh cache: %w", err)
	}

	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()

	seenDirs := make(map[string]bool)
	fileCount, dirCount := 0, 0
	
	for _, torrent := range f.cache.items {
		torrentEntries, torrentFiles, torrentDirs := f.processTorrentForListing(&torrent, dir, seenDirs)
		entries = append(entries, torrentEntries...)
		fileCount += torrentFiles
		dirCount += torrentDirs
	}

	fs.Debugf(f, "Listed %d files and %d directories in %q", fileCount, dirCount, dir)
	return entries, nil
}

// ensureFreshCache checks if cache needs refreshing and refreshes it if necessary
func (f *Fs) ensureFreshCache(ctx context.Context) error {
	f.cache.mu.Lock()
	needsRefresh := time.Since(f.cache.lastUpdated) > cacheExpiry
	cacheAge := time.Since(f.cache.lastUpdated)
	f.cache.mu.Unlock()
	
	if needsRefresh {
		fs.Debugf(f, "Cache is stale, refreshing")
		return f.refreshCache(ctx)
	}
	
	fs.Debugf(f, "Using cached data (age: %v)", cacheAge)
	return nil
}

// processTorrentForListing processes a single torrent and returns its entries for the given directory
func (f *Fs) processTorrentForListing(torrent *TorboxTorrent, dir string, seenDirs map[string]bool) (entries fs.DirEntries, fileCount, dirCount int) {
	for _, file := range torrent.Files {
		relativePath := f.getRelativePath(file.Name, dir)
		if relativePath == "" {
			continue
		}

		segments := strings.Split(relativePath, "/")
		
		if len(segments) == 1 {
			// File directly in current directory
			obj := f.createObject(torrent, &file, dir, relativePath)
			entries = append(entries, obj)
			fileCount++
		} else {
			// File in subdirectory - create directory entry for immediate subdirectory
			immediateSubdir := segments[0]
			if !seenDirs[immediateSubdir] {
				seenDirs[immediateSubdir] = true
				dirName := f.opt.Enc.ToStandardName(immediateSubdir)
				dirEntry := fs.NewDir(dirName, time.Time{})
				entries = append(entries, dirEntry)
				dirCount++
			}
		}
	}
	return entries, fileCount, dirCount
}

// getRelativePath calculates the relative path for a file within the specified directory
func (f *Fs) getRelativePath(fullPath, dir string) string {
	var relativePath string
	
	// Handle root directory filtering
	if f.root != "" {
		if !strings.HasPrefix(fullPath, f.root+"/") && fullPath != f.root {
			return ""
		}
		if fullPath == f.root {
			relativePath = ""
		} else {
			relativePath = fullPath[len(f.root)+1:]
		}
	} else {
		relativePath = fullPath
	}
	
	// Handle current directory filtering
	if dir != "" {
		dirWithSlash := dir + "/"
		if !strings.HasPrefix(relativePath, dirWithSlash) && relativePath != dir {
			return ""
		}
		if relativePath == dir {
			relativePath = ""
		} else {
			relativePath = relativePath[len(dirWithSlash):]
		}
	}
	
	return relativePath
}

// createObject creates a new Object from torrent and file information
func (f *Fs) createObject(torrent *TorboxTorrent, file *TorboxFile, dir, relativePath string) *Object {
	remote := path.Join(dir, relativePath)
	return &Object{
		fs:          f,
		remote:      remote,
		id:          fmt.Sprintf("%s:%d", torrent.Hash, file.ID),
		size:        file.Size,
		mimeType:    file.Mimetype,
		hasMetaData: true,
		fileID:      file.ID,
		hash:        torrent.Hash,
		s3Path:      file.S3Path,
		infected:    file.Infected,
		zipped:      file.Zipped,
	}
}

// refreshCache fetches the list of items from all relevant endpoints and updates the cache.
func (f *Fs) refreshCache(ctx context.Context) error {
	fs.Debugf(f, "torbox: Refreshing cache")
	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()

	f.cache.items = []TorboxTorrent{}

	// Get the list of torrents
	fs.Debugf(f, "torbox: Fetching torrent list from API")
	torrentListOpts := rest.Opts{
		Method:     "GET",
		Path:       "/torrents/mylist",
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + f.opt.APIKey,
		},
	}

	var torrentListResult struct {
		Success bool           `json:"success"`
		Error   string        `json:"error"`
		Detail  string        `json:"detail"`
		Data    []TorboxTorrent `json:"data"`
	}

	var resp *http.Response
	var err error
	callErr := f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &torrentListOpts, nil, &torrentListResult)
		return shouldRetry(ctx, resp, err)
	})
	if callErr != nil {
		fs.Errorf(f, "Failed to list torrents for cache refresh: %v", callErr)
		return callErr
	} else if !torrentListResult.Success {
		fs.Errorf(f, "Error listing torrents for cache refresh: %s", torrentListResult.Error+": "+torrentListResult.Detail)
		return errors.New(torrentListResult.Error + ": " + torrentListResult.Detail)
	}

	// Update cache with torrent data
	fs.Debugf(f, "torbox: Retrieved %d torrents from API", len(torrentListResult.Data))
	f.cache.items = append(f.cache.items, torrentListResult.Data...)
	f.cache.lastUpdated = time.Now()
	fs.Debugf(f, "torbox: Cache updated successfully with %d total items", len(f.cache.items))
	return nil
}

// NewObject finds the Object at remote.
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fs.Debugf(f, "torbox: Looking for object %q", remote)
	
	// Check if cache needs refreshing and do it outside the lock to avoid race conditions
	needsRefresh := false
	f.cache.mu.Lock()
	if time.Since(f.cache.lastUpdated) > time.Minute { // Cache expires after 1 minute
		needsRefresh = true
	}
	f.cache.mu.Unlock()
	
	if needsRefresh {
		fs.Debugf(f, "torbox: Cache is stale, refreshing for object search")
		err := f.refreshCache(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to refresh cache: %w", err)
		}
	}

	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()

	// Calculate the full path for the remote object
	var targetPath string
	if f.root != "" {
		targetPath = f.root + "/" + remote
	} else {
		targetPath = remote
	}

	// Search through all torrents and their files
	for _, torrent := range f.cache.items {
		fs.Debugf(f, "torbox: Searching in torrent %q", torrent.Name)
		for _, file := range torrent.Files {
			if file.Name == targetPath {
				// Found the object
				fs.Debugf(f, "torbox: Found object %q in torrent %q (file ID: %d, size: %d)", remote, torrent.Name, file.ID, file.Size)
				o := &Object{
					fs:          f,
					remote:      remote,
					id:          fmt.Sprintf("%s:%d", torrent.Hash, file.ID), // Fixed: use torrent.Hash for consistency
					size:        file.Size,
					mimeType:    file.Mimetype,
					hasMetaData: true,
					fileID:      file.ID,
					hash:        torrent.Hash, // Fixed: use torrent.Hash for consistency
					s3Path:      file.S3Path,
					infected:    file.Infected,
					zipped:      file.Zipped,
				}
				return o, nil
			}
		}
	}

	fs.Debugf(f, "torbox: Object %q not found", remote)
	return nil, fs.ErrorObjectNotFound
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	// The Torbox API does not have a dedicated endpoint for creating directories.
	// Directories are implicitly created when uploading files with paths.
	return fs.ErrorDirNotFound
}

// Rmdir deletes the root folder
// Returns an error if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	// The Torbox API does not have a dedicated endpoint for deleting directories.
	// Deletion is primarily at the torrent level.
	return fs.ErrorDirNotFound
}

// Purge deletes all the files in the directory
func (f *Fs) Purge(ctx context.Context, dir string) error {
	// The Torbox API does not support purging directories.
	// Deletion is primarily at the torrent level.
	return fs.ErrorCantPurge
}

// Move src to this remote using server-side move operations.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	// The Torbox API does not have a dedicated endpoint for moving or renaming files.
	return nil, fs.ErrorCantMove
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server-side move operations.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	// The Torbox API does not have a dedicated endpoint for moving or renaming directories.
	return fs.ErrorCantDirMove
}

// PublicLink adds a "readable by anyone with link" permission on the given file or folder.
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	fs.Debugf(f, "torbox: Generating public link for %q", remote)
	
	// The /v1/api/torrents/requestdl endpoint generates a download URL.
	// Assuming this URL is publicly accessible.

	// Find the object to get its ID
	obj, err := f.NewObject(ctx, remote)
	if err != nil {
		fs.Debugf(f, "torbox: Failed to find object %q for public link: %v", remote, err)
		return "", fmt.Errorf("failed to get object for public link: %w", err)
	}
	torboxObject, ok := obj.(*Object)
	if !ok {
		return "", errors.New("internal error: object is not a torbox object")
	}

	// Extract torrent ID and file path from the object ID
	parts := strings.SplitN(torboxObject.id, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("invalid object ID format")
	}
	torrentID := parts[0]
	filePath := parts[1]
	
	fs.Debugf(f, "torbox: Requesting download link for torrent %q, file %q", torrentID, filePath)

	// Call /v1/api/torrents/requestdl to get the download URL
	opts := rest.Opts{
		Method:     "GET",
		Path:       "/torrents/requestdl",
		Parameters: f.baseParams(),
	}
	opts.Parameters.Set("torrent_id", torrentID)
	opts.Parameters.Set("file_id", filePath) // Assuming file_id can be the file path
	opts.Parameters.Set("redirect", "false") // Get the URL instead of redirecting

	var result struct {
		Success bool `json:"success"`
		Error   string `json:"error"`
		Detail  string `json:"detail"`
		URL     string `json:"url"`
	}

	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		fs.Debugf(f, "torbox: Failed to get public link for %q: %v", remote, err)
		return "", fmt.Errorf("failed to get public link: %w", err)
	}
	if !result.Success {
		fs.Debugf(f, "torbox: API error getting public link for %q: %s", remote, result.Error+": "+result.Detail)
		return "", errors.New(result.Error + ": " + result.Detail)
	}

	fs.Debugf(f, "torbox: Generated public link for %q: %s", remote, result.URL)
	return result.URL, nil
}

// Shutdown shutdown the fs
func (f *Fs) Shutdown(ctx context.Context) error {
	// No specific shutdown needed for this backend
	return nil
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (usage *fs.Usage, err error) {
	// The Torbox API /v1/api/user/me endpoint does not provide quota information.
	return nil, fs.ErrorNotImplemented
}

// DirCacheFlush resets the directory cache - used in testing as an
// optional interface
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	// Need to confirm supported hashes with Torbox API
	return hash.Set(hash.None)
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Hash returns the SHA-1 of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	// Need to confirm supported hashes with Torbox API
	return "", hash.ErrUnsupported
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	fs.Debugf(o.fs, "torbox: Getting size for object %q: %d bytes", o.remote, o.size)
	return o.size
}

// ModTime returns the modification time of the object
func (o *Object) ModTime(ctx context.Context) time.Time {
	fs.Debugf(o.fs, "torbox: Getting mod time for object %q: %v", o.remote, o.modTime)
	return o.modTime
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	fs.Debugf(o.fs, "torbox: Attempted to set mod time for object %q (not supported)", o.remote)
	return fs.ErrorCantSetModTime
}

// Storable returns a boolean showing whether this object storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	fs.Debugf(o.fs, "torbox: Opening object %q for reading", o.remote)
	
	// Extract hash and file ID from the object ID
	parts := strings.SplitN(o.id, ":", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid object ID format")
	}
	hash := parts[0]
	fileID := parts[1]
	
	fs.Debugf(o.fs, "torbox: Requesting download URL for hash %q, file ID %q", hash, fileID)

	// Call /v1/api/torrents/requestdl to get the download URL
	opts := rest.Opts{
		Method:     "GET",
		Path:       "/torrents/requestdl",
		Parameters: o.fs.baseParams(),
	}
	opts.Parameters.Set("torrent_id", hash)
	opts.Parameters.Set("file_id", fileID)
	opts.Parameters.Set("zip_link", "false")  // Default to false per API spec
	opts.Parameters.Set("redirect", "false")  // We want the URL, not a redirect

	var result struct {
		Success bool `json:"success"`
		Error   string `json:"error"`
		Detail  string `json:"detail"`
		URL     string `json:"url"`
	}

	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		fs.Debugf(o.fs, "torbox: Failed to get download URL for %q: %v", o.remote, err)
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}
	if !result.Success {
		fs.Debugf(o.fs, "torbox: API error getting download URL for %q: %s", o.remote, result.Error+": "+result.Detail)
		return nil, errors.New(result.Error + ": " + result.Detail)
	}

	fs.Debugf(o.fs, "torbox: Got download URL for %q: %s", o.remote, result.URL)

	// Perform GET request to the download URL
	downloadOpts := rest.Opts{
		Method:  "GET",
		RootURL: result.URL,
		Options: options,
	}

	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.Call(ctx, &downloadOpts)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		fs.Debugf(o.fs, "torbox: Failed to download object %q: %v", o.remote, err)
		return nil, fmt.Errorf("failed to open object: %w", err)
	}

	fs.Debugf(o.fs, "torbox: Successfully opened object %q for reading", o.remote)
	return resp.Body, nil
}

// Update the object with the contents of the io.Reader, modTime and size
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	// Need to implement updating object based on Torbox API
	// The API spec doesn't explicitly show an update endpoint.
	// Need to investigate if uploading a new file with the same name overwrites the existing one.
	return errors.New("Update not implemented yet")
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o.fs, "torbox: Removing object %q", o.remote)
	
	// Extract item ID and type from the object ID
	parts := strings.SplitN(o.id, ":", 2)
	if len(parts) != 2 {
		return errors.New("invalid object ID format")
	}
	itemID := parts[0]
	itemType := parts[1]
	
	fs.Debugf(o.fs, "torbox: Attempting to remove item ID %q of type %q", itemID, itemType)

	var resp *http.Response
	var result struct {
		Success bool `json:"success"`
		Error   string `json:"error"`
		Detail  string `json:"detail"`
	}

	// Call the appropriate delete endpoint based on item type
	switch itemType {
	case "torrent":
		fs.Debugf(o.fs, "torbox: Deleting torrent %q", itemID)
		opts := rest.Opts{
			Method: "POST",
			Path:   "/torrents/controltorrent",
			Parameters: o.fs.baseParams(),
			Body: strings.NewReader(fmt.Sprintf(`{"operation": "delete", "torrent_id": %s}`, itemID)),
			ContentType: "application/json",
		}
		var err error
		err = o.fs.pacer.Call(func() (bool, error) {
			resp, err = o.fs.srv.CallJSON(ctx, &opts, nil, &result)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			fs.Debugf(o.fs, "torbox: Failed to delete torrent %q: %v", itemID, err)
			return fmt.Errorf("failed to delete torrent: %w", err)
		}
		if !result.Success {
			fs.Debugf(o.fs, "torbox: API error deleting torrent %q: %s", itemID, result.Error+": "+result.Detail)
			return errors.New(result.Error + ": " + result.Detail)
		}

	case "usenet":
		fs.Debugf(o.fs, "torbox: Deleting usenet download %q", itemID)
		opts := rest.Opts{
			Method: "POST",
			Path:   "/usenet/controlusenetdownload",
			Parameters: o.fs.baseParams(),
			Body: strings.NewReader(fmt.Sprintf(`{"operation": "delete", "usenet_id": %s}`, itemID)),
			ContentType: "application/json",
		}
		var err error
		err = o.fs.pacer.Call(func() (bool, error) {
			resp, err = o.fs.srv.CallJSON(ctx, &opts, nil, &result)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			fs.Debugf(o.fs, "torbox: Failed to delete usenet download %q: %v", itemID, err)
			return fmt.Errorf("failed to delete usenet download: %w", err)
		}
		if !result.Success {
			fs.Debugf(o.fs, "torbox: API error deleting usenet download %q: %s", itemID, result.Error+": "+result.Detail)
			return errors.New(result.Error + ": " + result.Detail)
		}

	case "webdl":
		fs.Debugf(o.fs, "torbox: Deleting web download %q", itemID)
		opts := rest.Opts{
			Method: "POST",
			Path:   "/webdl/controlwebdownload",
			Parameters: o.fs.baseParams(),
			Body: strings.NewReader(fmt.Sprintf(`{"operation": "delete", "webdl_id": %s}`, itemID)),
			ContentType: "application/json",
		}
		var err error
		err = o.fs.pacer.Call(func() (bool, error) {
			resp, err = o.fs.srv.CallJSON(ctx, &opts, nil, &result)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			fs.Debugf(o.fs, "torbox: Failed to delete web download %q: %v", itemID, err)
			return fmt.Errorf("failed to delete web download: %w", err)
		}
		if !result.Success {
			fs.Debugf(o.fs, "torbox: API error deleting web download %q: %s", itemID, result.Error+": "+result.Detail)
			return errors.New(result.Error + ": " + result.Detail)
		}

	default:
		fs.Debugf(o.fs, "torbox: Unsupported item type for deletion: %q", itemType)
		return errors.New("unsupported item type for deletion")
	}

	// Invalidate the cache after deletion
	fs.Debugf(o.fs, "torbox: Invalidating cache after successful deletion of %q", o.remote)
	o.fs.cache.mu.Lock()
	o.fs.cache.lastUpdated = time.Time{} // Invalidate cache
	o.fs.cache.mu.Unlock()

	fs.Debugf(o.fs, "torbox: Successfully removed object %q", o.remote)
	return nil
}

// MimeType of an Object if known, "" otherwise
func (o *Object) MimeType(ctx context.Context) string {
	// Need to implement getting mime type - API doesn't seem to provide it directly in list
	return ""
}

// ID returns the ID of the Object if known, or "" if not
func (o *Object) ID() string {
	// Need to implement getting ID - using combined item ID and type for now
	return o.id
}

// Put in to the remote path with the modTime given of the given size
//
// May create the object even if it returns an error - if so
// will return the object and the error, otherwise will return
// nil and the error
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf(f, "torbox: Attempted to put object %q (not supported - Torbox doesn't support direct uploads)", src.Remote())
	// Torbox API doesn't support direct file uploads
	// Files must be added through torrents, usenet, or web downloads
	return nil, fs.ErrorNotImplemented
}

// Check the interfaces are satisfied
var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
	// Uncomment and add more interfaces as they are implemented
	// _ fs.Purger          = (*Fs)(nil)
	// _ fs.Mover           = (*Fs)(nil)
	// _ fs.DirMover        = (*Fs)(nil)
	// _ fs.DirCacheFlusher = (*Fs)(nil)
	// _ fs.Abouter         = (*Fs)(nil)
	_ fs.PublicLinker    = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	// _ fs.MimeTyper       = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
)
