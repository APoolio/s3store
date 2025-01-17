package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	cm "github.com/caddyserver/certmagic"
	"github.com/rs/zerolog"
)

const (
	loadMethod           = "load"
	existsMethod         = "exists"
	storeMethod          = "store"
	deleteMethod         = "delete"
	listMethod           = "list"
	statMethod           = "stat"
	lockMethod           = "lock"
	createLockFileMethod = "createLockFile"
	deleteLockFileMethod = "deleteLockFile"
	method               = "source"
)

const lockFileExists = "Lock file for already exists"

// staleLockDuration is the length of time
// before considering a lock to be stale.
const staleLockDuration = 2 * time.Hour

// fileLockPollInterval is how frequently
// to check the existence of a lock file
const fileLockPollInterval = 1 * time.Second

var StorageKeys cm.KeyBuilder

// S3Storage implements the certmagic Storage interface using amazon's
// s3 storage.  An effort has been made to make the S3Storage implementation
// as similar as possible to the original filestorage type in order to
// provide a consistent approach to storage backends for certmagic
// for issues, please contact @securityclippy
// S3Storage is safe to use with multiple servers behind an AWS load balancer
// and is safe for concurrent use

type S3Store struct {
	prefix string
	bucket *string
	client *s3.Client
	log    zerolog.Logger
}

func NewS3Store(log zerolog.Logger, client *s3.Client, bucketName string) *S3Store {

	store := &S3Store{
		bucket: aws.String(bucketName),
		client: client,
		prefix: "certmagic",
		log:    log,
	}

	return store
}

func NewS3StoreWithCredentials(accessKey, secretKey, bucketName, region string) *S3Store {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		config.WithRegion(region),
	)
	if err != nil {
		log.Fatal(err)
	}
	client := s3.NewFromConfig(cfg)
	store := &S3Store{
		bucket: aws.String(bucketName),
		client: client,
		prefix: "certmagic",
	}

	return store
}

// Exists returns true if key exists in s3
func (s *S3Store) Exists(ctx context.Context, key string) bool {
	input := &s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(s.Filename(key)),
	}
	_, err := s.client.GetObject(ctx, input)
	if err == nil {
		return true
	}
	s.log.Error().Err(err).Str(method, existsMethod).Msgf("key does not exist in s3 (%s)", key)

	var nsk *types.NoSuchKey
	return !errors.As(err, &nsk)
}

// Store saves value at key.
func (s *S3Store) Store(ctx context.Context, key string, value []byte) error {
	filename := s.Filename(key)
	input := &s3.PutObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(filename),
		Body:   bytes.NewReader(value),
	}
	_, err := s.client.PutObject(ctx, input)

	if err != nil {
		s.log.Error().Err(err).Str(method, storeMethod).Msg("error encountered calling putObject to s3")
		return err
	}
	return nil
}

// Load retrieves the value at key.
func (s *S3Store) Load(ctx context.Context, key string) ([]byte, error) {
	input := &s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(s.Filename(key)),
	}
	result, err := s.client.GetObject(ctx, input)
	if err != nil {
		s.log.Error().Err(err).Str(method, loadMethod).Msgf("failed fetching value from s3 (key: %s)", key)
		return nil, err
	}

	b, err := ioutil.ReadAll(result.Body)
	if err != nil {
		s.log.Error().Err(err).Str(method, loadMethod).Msg("error encountered reading result")
		return nil, err
	}
	return b, nil
}

// Delete deletes the value at key.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	input := &s3.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(s.Filename(key)),
	}
	_, err := s.client.DeleteObject(ctx, input)
	if err != nil {
		s.log.Error().Err(err).Str(method, deleteMethod).Msg("error deleting object")
		return err
	}
	return nil
}

// List returns all keys that match prefix.
// because s3 has no concept of directories, everything is an explicit path,
// there is really no such thing as recursive search. This is simply
// here to fulfill the interface requirements of the List function
func (s *S3Store) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	var keys []string
	prefixPath := s.Filename(prefix)
	input := &s3.ListObjectsInput{
		Bucket: s.bucket,
		Prefix: aws.String(prefixPath),
	}

	result, err := s.client.ListObjects(ctx, input)
	if err != nil {
		s.log.Error().Err(err).Str(method, listMethod).Msg("error encountered while listing objects in s3")
		return nil, err
	}
	for _, k := range result.Contents {
		if strings.HasPrefix(*k.Key, prefix) {
			keys = append(keys, *k.Key)
		}
	}
	//
	return keys, nil
}

// Stat returns information about key.
func (s *S3Store) Stat(ctx context.Context, key string) (cm.KeyInfo, error) {
	input := &s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(key),
	}
	result, err := s.client.GetObject(ctx, input)

	if err != nil {
		return cm.KeyInfo{}, err
	}

	return cm.KeyInfo{
		Key:        key,
		Size:       result.ContentLength,
		Modified:   *result.LastModified,
		IsTerminal: true,
	}, nil
}

// Filename returns the key as a path on the file
// system prefixed by S3Storage.Path.
func (s *S3Store) Filename(key string) string {
	return filepath.Join(s.prefix, filepath.FromSlash(key))
}

// Lock obtains a lock named by the given key. It blocks
// until the lock can be obtained or an error is returned.
func (s *S3Store) Lock(ctx context.Context, key string) error {
	start := time.Now()
	lockFile := s.lockFileName(key)

	for {
		err := s.createLockFile(ctx, lockFile)
		if err == nil {
			// got the lock, yay
			return nil
		}

		if err.Error() != lockFileExists {
			// unexpected error
			s.log.Error().Err(err).Str(method, createLockFileMethod).Msg("unexpected error")
			fmt.Println(err)
			return fmt.Errorf("creating lock file: %+v", err)

		}

		// lock file already exists

		info, err := s.Stat(ctx, lockFile)
		switch {
		case s.errNoSuchKey(err):
			// must have just been removed; try again to create it
			s.log.Error().Err(err).Str(method, statMethod).Msg("no such key")
			continue

		case err != nil:
			// unexpected error
			lockErr := fmt.Errorf("accessing lock file: %v", err)
			s.log.Error().Err(lockErr).Str(method, statMethod).Msg("stat err")
			return lockErr

		case s.fileLockIsStale(info):
			log.Printf("[INFO][%s] Lock for '%s' is stale; removing then retrying: %s",
				s, key, lockFile)
			deleteErr := s.deleteLockFile(ctx, lockFile)
			if deleteErr != nil {
				s.log.Error().Err(deleteErr).Str(method, deleteLockFileMethod).Msg("file lock is stale")
			}
			continue

		case time.Since(start) > staleLockDuration*2:
			// should never happen, hopefully
			staleLockErr := fmt.Errorf("possible deadlock: %s passed trying to obtain lock for %s",
				time.Since(start), key)
			s.log.Error().Err(staleLockErr).Str(method, lockMethod).Msg("stale lock duration")
			return staleLockErr

		default:
			// lockfile exists and is not stale;
			// just wait a moment and try again
			time.Sleep(fileLockPollInterval)

		}
	}
}

// Unlock releases the lock for name.
func (s *S3Store) Unlock(ctx context.Context, key string) error {
	return s.deleteLockFile(ctx, s.lockFileName(key))
}

func (s *S3Store) String() string {
	return "S3Storage:" + s.prefix
}

func (s *S3Store) lockFileName(key string) string {
	return filepath.Join(s.lockDir(), StorageKeys.Safe(key)+".lock")
}

func (s *S3Store) lockDir() string {
	return filepath.Join(s.prefix, "locks")
}

func (s *S3Store) fileLockIsStale(info cm.KeyInfo) bool {
	return time.Since(info.Modified) > staleLockDuration
}

func (s *S3Store) createLockFile(ctx context.Context, filename string) error {
	//lf := s.lockFileName(key)
	exists := s.Exists(ctx, filename)
	if exists {
		return fmt.Errorf(lockFileExists)
	}
	input := &s3.PutObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(filename),
		Body:   bytes.NewReader([]byte("lock")),
	}
	_, err := s.client.PutObject(ctx, input)

	if err != nil {
		return err
	}
	return nil
}

func (s *S3Store) deleteLockFile(ctx context.Context, keyPath string) error {
	input := &s3.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(keyPath),
	}
	_, err := s.client.DeleteObject(ctx, input)
	if err != nil {
		return err
	}
	return nil
}

func (s *S3Store) errNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if err != nil {
		if errors.As(err, &nsk) {
			return true
		}
	}
	return false
}
