// Package dynamodb contains the DynamoDB store implementation.
package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/gorilla/websocket"
	"github.com/kvtools/valkeyrie"
	"github.com/kvtools/valkeyrie/store"
)

// StoreName the name of the store.
const StoreName = "dynamodb"

const (
	// DefaultReadCapacityUnits default read capacity used to create table.
	DefaultReadCapacityUnits = 2
	// DefaultWriteCapacityUnits default write capacity used to create table.
	DefaultWriteCapacityUnits = 2
	// DeleteTreeTimeoutSeconds the maximum time we retry a write batch.
	DeleteTreeTimeoutSeconds = 30
)

const (
	partitionKey          = "id"
	revisionAttribute     = "version"
	encodedValueAttribute = "encoded_value"
	ttlAttribute          = "expiration_time"
)

const (
	defaultLockTTL         = 20 * time.Second
	dynamodbDefaultTimeout = 10 * time.Second
)

var (
	// ErrBucketOptionMissing is returned when bucket config option is missing.
	ErrBucketOptionMissing = errors.New("missing dynamodb bucket/table name")
	// ErrMultipleEndpointsUnsupported is returned when more than one endpoint is provided.
	ErrMultipleEndpointsUnsupported = errors.New("dynamodb only supports one endpoint")
	// ErrDeleteTreeTimeout delete batch timed out.
	ErrDeleteTreeTimeout = errors.New("delete batch timed out")
	// ErrLockAcquireCancelled stop called before lock was acquired.
	ErrLockAcquireCancelled = errors.New("stop called before lock was acquired")
)

// Register register a store provider in valkeyrie for AWS DynamoDB.

// registers AWS DynamoDB to Valkeyrie.
func init() {
	valkeyrie.Register(StoreName, newStore)
}

// Config the AWS DynamoDB configuration.
type Config struct {
	Bucket string
	Region *string
}

func newStore(ctx context.Context, endpoints []string, options valkeyrie.Config) (store.Store, error) {
	cfg, ok := options.(*Config)
	if !ok && options != nil {
		return nil, &store.InvalidConfigurationError{Store: StoreName, Config: options}
	}

	return New(ctx, endpoints, cfg)
}

// Store implements the store.Store interface.
type Store struct {
	dynamoSvc dynamodbiface.DynamoDBAPI
	tableName string
}

// New creates a new AWS DynamoDB client.
func New(_ context.Context, endpoints []string, options *Config) (*Store, error) {
	if len(endpoints) > 1 {
		return nil, ErrMultipleEndpointsUnsupported
	}

	if options == nil || options.Bucket == "" {
		return nil, ErrBucketOptionMissing
	}
	var config *aws.Config = &aws.Config{}
	if len(endpoints) == 1 {
		config.Endpoint = aws.String(endpoints[0])
	}
	if options.Region != nil && *options.Region != "" {
		config.Region = options.Region
	}

	ddb := &Store{
		dynamoSvc: dynamodb.New(session.Must(session.NewSession(config))),
		tableName: options.Bucket,
	}

	return ddb, nil
}

// Put a value at the specified key.
func (ddb *Store) Put(ctx context.Context, key string, value []byte, opts *store.WriteOptions) error {
	keyAttr := make(map[string]*dynamodb.AttributeValue)
	keyAttr[partitionKey] = &dynamodb.AttributeValue{S: aws.String(key)}

	exAttr := map[string]*dynamodb.AttributeValue{
		":incr": {N: aws.String("1")},
	}

	var setList []string

	// if a value was provided append it to the update expression.
	if len(value) > 0 {
		encodedValue := base64.StdEncoding.EncodeToString(value)
		exAttr[":encv"] = &dynamodb.AttributeValue{S: aws.String(encodedValue)}
		setList = append(setList, fmt.Sprintf("%s = :encv", encodedValueAttribute))
	}

	// if a ttl was provided validate it and append it to the update expression.
	if opts != nil && opts.TTL > 0 {
		ttlVal := time.Now().Add(opts.TTL).Unix()
		exAttr[":ttl"] = &dynamodb.AttributeValue{N: aws.String(strconv.FormatInt(ttlVal, 10))}
		setList = append(setList, fmt.Sprintf("%s = :ttl", ttlAttribute))
	}

	updateExp := fmt.Sprintf("ADD %s :incr", revisionAttribute)

	if len(setList) > 0 {
		updateExp = fmt.Sprintf("%s SET %s", updateExp, strings.Join(setList, ","))
	}

	_, err := ddb.dynamoSvc.UpdateItemWithContext(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(ddb.tableName),
		Key:                       keyAttr,
		ExpressionAttributeValues: exAttr,
		UpdateExpression:          aws.String(updateExp),
	})
	if err != nil {
		return err
	}

	return nil
}

// Get a value given its key.
func (ddb *Store) Get(ctx context.Context, key string, opts *store.ReadOptions) (*store.KVPair, error) {
	if opts == nil {
		opts = &store.ReadOptions{
			Consistent: true, // default to enabling read consistency.
		}
	}

	res, err := ddb.getKey(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	if res.Item == nil {
		return nil, store.ErrKeyNotFound
	}

	// is the item expired?
	if isItemExpired(res.Item) {
		return nil, store.ErrKeyNotFound
	}

	return decodeItem(res.Item)
}

func (ddb *Store) getKey(ctx context.Context, key string, options *store.ReadOptions) (*dynamodb.GetItemOutput, error) {
	return ddb.dynamoSvc.GetItemWithContext(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(ddb.tableName),
		ConsistentRead: aws.Bool(options.Consistent),
		Key: map[string]*dynamodb.AttributeValue{
			partitionKey: {S: aws.String(key)},
		},
	})
}

// Delete the value at the specified key.
func (ddb *Store) Delete(ctx context.Context, key string) error {
	_, err := ddb.dynamoSvc.DeleteItemWithContext(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(ddb.tableName),
		Key: map[string]*dynamodb.AttributeValue{
			partitionKey: {S: aws.String(key)},
		},
	})
	if err != nil {
		return err
	}

	return nil
}

// Exists if a Key exists in the store.
func (ddb *Store) Exists(ctx context.Context, key string, _ *store.ReadOptions) (bool, error) {
	res, err := ddb.dynamoSvc.GetItemWithContext(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(ddb.tableName),
		Key: map[string]*dynamodb.AttributeValue{
			partitionKey: {
				S: aws.String(key),
			},
		},
	})
	if err != nil {
		return false, err
	}

	if res.Item == nil {
		return false, nil
	}

	// is the item expired?
	if isItemExpired(res.Item) {
		return false, nil
	}

	return true, nil
}

// List the content of a given prefix.
func (ddb *Store) List(ctx context.Context, directory string, opts *store.ReadOptions) ([]*store.KVPair, error) {
	if opts == nil {
		opts = &store.ReadOptions{
			Consistent: true, // default to enabling read consistency.
		}
	}

	expAttr := make(map[string]*dynamodb.AttributeValue)
	expAttr[":namePrefix"] = &dynamodb.AttributeValue{S: aws.String(directory)}

	filterExp := fmt.Sprintf("begins_with(%s, :namePrefix)", partitionKey)

	si := &dynamodb.ScanInput{
		TableName:                 aws.String(ddb.tableName),
		FilterExpression:          aws.String(filterExp),
		ExpressionAttributeValues: expAttr,
		ConsistentRead:            aws.Bool(opts.Consistent),
	}

	var items []map[string]*dynamodb.AttributeValue
	ctx, cancel := context.WithTimeout(ctx, dynamodbDefaultTimeout)

	err := ddb.dynamoSvc.ScanPagesWithContext(ctx, si,
		func(page *dynamodb.ScanOutput, lastPage bool) bool {
			items = append(items, page.Items...)

			if lastPage {
				cancel()
				return false
			}

			return true
		})
	if err != nil {
		return nil, err
	}

	if len(items) == 0 {
		return nil, store.ErrKeyNotFound
	}

	var kvArray []*store.KVPair
	var val *store.KVPair

	for _, item := range items {
		val, err = decodeItem(item)
		if err != nil {
			return nil, err
		}

		// skip the records which match the prefix.
		if val.Key == directory {
			continue
		}
		// skip records which are expired.
		if isItemExpired(item) {
			continue
		}

		kvArray = append(kvArray, val)
	}

	return kvArray, nil
}

// DeleteTree deletes a range of keys under a given directory.
func (ddb *Store) DeleteTree(ctx context.Context, keyPrefix string) error {
	expAttr := make(map[string]*dynamodb.AttributeValue)

	expAttr[":namePrefix"] = &dynamodb.AttributeValue{S: aws.String(keyPrefix)}

	res, err := ddb.dynamoSvc.ScanWithContext(ctx, &dynamodb.ScanInput{
		TableName:                 aws.String(ddb.tableName),
		FilterExpression:          aws.String(fmt.Sprintf("begins_with(%s, :namePrefix)", partitionKey)),
		ExpressionAttributeValues: expAttr,
	})
	if err != nil {
		return err
	}

	if len(res.Items) == 0 {
		return nil
	}

	items := make(map[string][]*dynamodb.WriteRequest)

	items[ddb.tableName] = make([]*dynamodb.WriteRequest, len(res.Items))

	for n, item := range res.Items {
		items[ddb.tableName][n] = &dynamodb.WriteRequest{
			DeleteRequest: &dynamodb.DeleteRequest{
				Key: map[string]*dynamodb.AttributeValue{
					partitionKey: item[partitionKey],
				},
			},
		}
	}

	return ddb.retryDeleteTree(ctx, items)
}

// AtomicPut Atomic CAS operation on a single value.
func (ddb *Store) AtomicPut(ctx context.Context, key string, value []byte, previous *store.KVPair, opts *store.WriteOptions) (bool, *store.KVPair, error) {
	getRes, err := ddb.getKey(ctx, key, &store.ReadOptions{
		Consistent: true, // enable the read consistent flag.
	})
	if err != nil {
		return false, nil, err
	}

	// AtomicPut is equivalent to Put if previous is nil and the Key exist in the DB or is not expired.
	if previous == nil && getRes.Item != nil && !isItemExpired(getRes.Item) {
		return false, nil, store.ErrKeyExists
	}

	keyAttr := make(map[string]*dynamodb.AttributeValue)
	keyAttr[partitionKey] = &dynamodb.AttributeValue{S: aws.String(key)}

	exAttr := make(map[string]*dynamodb.AttributeValue)
	exAttr[":incr"] = &dynamodb.AttributeValue{N: aws.String("1")}

	var setList []string

	// if a value was provided append it to the update expression.
	if len(value) > 0 {
		encodedValue := base64.StdEncoding.EncodeToString(value)
		exAttr[":encv"] = &dynamodb.AttributeValue{S: aws.String(encodedValue)}
		setList = append(setList, fmt.Sprintf("%s = :encv", encodedValueAttribute))
	}

	// if a ttl was provided validate it and append it to the update expression.
	if opts != nil && opts.TTL > 0 {
		ttlVal := time.Now().Add(opts.TTL).Unix()
		exAttr[":ttl"] = &dynamodb.AttributeValue{N: aws.String(strconv.FormatInt(ttlVal, 10))}
		setList = append(setList, fmt.Sprintf("%s = :ttl", ttlAttribute))
	}

	updateExp := fmt.Sprintf("ADD %s :incr", revisionAttribute)

	if len(setList) > 0 {
		updateExp = fmt.Sprintf("%s SET %s", updateExp, strings.Join(setList, ","))
	}

	var condExp *string

	if previous != nil {
		exAttr[":lastRevision"] = &dynamodb.AttributeValue{N: aws.String(strconv.FormatUint(previous.LastIndex, 10))}
		exAttr[":timeNow"] = &dynamodb.AttributeValue{N: aws.String(strconv.FormatInt(time.Now().Unix(), 10))}

		// the previous kv is in the DB and is at the expected revision, also if it has a TTL set it is NOT expired.
		condExp = aws.String(fmt.Sprintf("%s = :lastRevision AND (attribute_not_exists(%s) OR (attribute_exists(%s) AND %s > :timeNow))",
			revisionAttribute, ttlAttribute, ttlAttribute, ttlAttribute))
	}

	res, err := ddb.dynamoSvc.UpdateItemWithContext(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(ddb.tableName),
		Key:                       keyAttr,
		ExpressionAttributeValues: exAttr,
		UpdateExpression:          aws.String(updateExp),
		ConditionExpression:       condExp,
		ReturnValues:              aws.String(dynamodb.ReturnValueAllNew),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == dynamodb.ErrCodeConditionalCheckFailedException {
				return false, nil, store.ErrKeyModified
			}
		}
		return false, nil, err
	}

	item, err := decodeItem(res.Attributes)
	if err != nil {
		return false, nil, err
	}

	return true, item, nil
}

// AtomicDelete delete of a single value.
func (ddb *Store) AtomicDelete(ctx context.Context, key string, previous *store.KVPair) (bool, error) {
	getRes, err := ddb.getKey(ctx, key, &store.ReadOptions{
		Consistent: true, // enable the read consistent flag.
	})
	if err != nil {
		return false, err
	}

	if previous == nil && getRes.Item != nil && !isItemExpired(getRes.Item) {
		return false, store.ErrKeyExists
	}

	expAttr := make(map[string]*dynamodb.AttributeValue)
	if previous != nil {
		expAttr[":lastRevision"] = &dynamodb.AttributeValue{N: aws.String(strconv.FormatUint(previous.LastIndex, 10))}
	}

	req := &dynamodb.DeleteItemInput{
		TableName: aws.String(ddb.tableName),
		Key: map[string]*dynamodb.AttributeValue{
			partitionKey: {S: aws.String(key)},
		},
		ConditionExpression:       aws.String(fmt.Sprintf("%s = :lastRevision", revisionAttribute)),
		ExpressionAttributeValues: expAttr,
	}

	_, err = ddb.dynamoSvc.DeleteItemWithContext(ctx, req)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == dynamodb.ErrCodeConditionalCheckFailedException {
				return false, store.ErrKeyNotFound
			}
		}
		return false, err
	}

	return true, nil
}

// Close nothing to see here.
func (ddb *Store) Close() error { return nil }

// NewLock has to implemented at the library level since it's not supported by DynamoDB.
func (ddb *Store) NewLock(_ context.Context, key string, opts *store.LockOptions) (store.Locker, error) {
	ttl := defaultLockTTL
	var value []byte
	renewCh := make(chan struct{})

	if opts != nil {
		if opts.TTL != 0 {
			ttl = opts.TTL
		}

		if len(opts.Value) != 0 {
			value = opts.Value
		}

		if opts.RenewLock != nil {
			renewCh = opts.RenewLock
		}
	}

	return &dynamodbLock{
		ddb:      ddb,
		last:     nil,
		key:      key,
		value:    value,
		ttl:      ttl,
		renewCh:  renewCh,
		unlockCh: make(chan struct{}),
	}, nil
}

// Watch has to implemented at the library level since it's not supported by DynamoDB.
func (ddb *Store) Watch(ctx context.Context, key string, _ *store.ReadOptions) (<-chan *store.KVPair, error) {
	watchCh := make(chan *store.KVPair)
	nKey := key

	get := getter(func() (interface{}, error) {
		// TODO: Take store.ReadOptions from parameters?
		pair, err := ddb.Get(ctx, nKey, nil)
		if err != nil {
			return nil, err
		}
		return pair, nil
	})

	push := pusher(func(v interface{}) {
		if val, ok := v.(*store.KVPair); ok {
			watchCh <- val
		}
	})

	sub, err := newSubscribe(ctx, nKey)
	if err != nil {
		return nil, err
	}

	go func(ctx context.Context, sub *subscribe, get getter, push pusher) {
		defer func() {
			close(watchCh)
			_ = sub.Close()
		}()

		msgCh := sub.Receive(ctx)
		if err := watchLoop(ctx, msgCh, get, push); err != nil {
			log.Printf("watchLoop in Watch err: %v", err)
		}
		log.Printf("Watch loop finished")
	}(ctx, sub, get, push)

	return watchCh, nil
}

// WatchTree has to implemented at the library level since it's not supported by DynamoDB.
func (ddb *Store) WatchTree(_ context.Context, _ string, _ *store.ReadOptions) (<-chan []*store.KVPair, error) {
	return nil, store.ErrCallNotSupported
}

// getter defines a func type which retrieves data from remote storage.
type getter func() (interface{}, error)

// pusher defines a func type which pushes data blob into watch channel.
type pusher func(interface{})

func watchLoop(ctx context.Context, msgCh chan *string, get getter, push pusher) error {
	// deliver the original data before we set up any events.
	pair, err := get()
	if err != nil && !errors.Is(err, store.ErrKeyNotFound) {
		return err
	}

	if errors.Is(err, store.ErrKeyNotFound) {
		pair = &store.KVPair{}
	}

	push(pair)

	log.Printf("Waiting for msg in watchLoop")
	for m := range msgCh {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// retrieve and send back.
		pair, err := get()
		if err != nil && !errors.Is(err, store.ErrKeyNotFound) {
			return err
		}

		// in case of watching a key that has been expired or deleted return and empty KV.
		//if errors.Is(err, store.ErrKeyNotFound) && (m.Payload == "expired" || m.Payload == "del") {
		if errors.Is(err, store.ErrKeyNotFound) && (*m == "expired" || *m == "del") {
			pair = &store.KVPair{}
		}

		push(pair)
	}
	log.Printf("no more msg in watchLoop")

	return nil
}

type subscribe struct {
	websocket *websocket.Conn
	closeCh   chan struct{}
}

func newSubscribe(ctx context.Context, key string) (*subscribe, error) {

	// connect to WSS server
	//var addr = flag.String("addr", "0dub4qh1di.execute-api.eu-central-1.amazonaws.com", "http service address")
	addr := "0dub4qh1di.execute-api.eu-central-1.amazonaws.com"

	u := url.URL{Scheme: "wss", Host: addr, Path: "/dev"}
	log.Printf("connecting to %s", u.String())

	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
		return nil, err
	}
	log.Printf("connected to %s", u.String())

	// subscribe to key
	msg := map[string]string{
		"action":    "subscribeChannel",
		"channelId": key,
	}
	jsonStr, err := json.Marshal(msg)
	if err != nil {
		log.Println("Error: " + err.Error())
		return nil, err
	}
	err = c.WriteMessage(websocket.TextMessage, []byte(jsonStr))
	if err != nil {
		log.Println("write:", err)
		return nil, err
	}

	return &subscribe{
		websocket: c,
		closeCh:   make(chan struct{}),
	}, nil
}

func (s *subscribe) Close() error {
	close(s.closeCh)
	return s.websocket.Close()
}

func (s *subscribe) Receive(ctx context.Context) chan *string {
	msgCh := make(chan *string)
	go s.receiveLoop(ctx, msgCh)
	return msgCh
}

func (s *subscribe) receiveLoop(ctx context.Context, msgCh chan *string) {
	defer close(msgCh)

	for {
		select {
		case <-s.closeCh:
			return
		case <-ctx.Done():
			return
		default:
			_, msg, err := s.websocket.ReadMessage()
			if err != nil {
				return
			}
			if msg != nil {
				log.Printf("received message")
				var jsonObject map[string]interface{}
				err = json.Unmarshal(msg, &jsonObject)
				if err != nil {
					log.Printf("Unmarshal failed in receiveLoop")
					return
				}
				message, ok := jsonObject["event"].(string)
				if !ok {
					log.Printf("Msg conversion failed in receiveLoop")
					return
				}
				log.Printf("message: %s", message)
				msgCh <- &(message)
			}
		}
	}
}

func (ddb *Store) createTable() error {
	_, err := ddb.dynamoSvc.CreateTable(&dynamodb.CreateTableInput{
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			{
				AttributeName: aws.String(partitionKey),
				AttributeType: aws.String("S"),
			},
		},
		KeySchema: []*dynamodb.KeySchemaElement{
			{
				AttributeName: aws.String(partitionKey),
				KeyType:       aws.String(dynamodb.KeyTypeHash),
			},
		},
		// enable encryption of data by default.
		SSESpecification: &dynamodb.SSESpecification{
			Enabled: aws.Bool(true),
			SSEType: aws.String(dynamodb.SSETypeAes256),
		},
		ProvisionedThroughput: &dynamodb.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Int64(DefaultReadCapacityUnits),
			WriteCapacityUnits: aws.Int64(DefaultWriteCapacityUnits),
		},
		TableName: aws.String(ddb.tableName),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == dynamodb.ErrCodeResourceInUseException {
				return nil
			}
		}
		return err
	}

	err = ddb.dynamoSvc.WaitUntilTableExists(&dynamodb.DescribeTableInput{
		TableName: aws.String(ddb.tableName),
	})
	if err != nil {
		return err
	}

	return nil
}

func (ddb *Store) retryDeleteTree(ctx context.Context, items map[string][]*dynamodb.WriteRequest) error {
	batchResult, err := ddb.dynamoSvc.BatchWriteItemWithContext(ctx, &dynamodb.BatchWriteItemInput{
		RequestItems: items,
	})
	if err != nil {
		return err
	}

	if len(batchResult.UnprocessedItems) == 0 {
		return nil
	}

	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(DeleteTreeTimeoutSeconds * time.Second)
		timeout <- true
	}()

	ticker := time.NewTicker(1 * time.Second)

	defer ticker.Stop()

	// Poll once a second for table status,
	// until the table is either active or the timeout deadline has been reached.
	for {
		select {
		case <-ticker.C:
			batchResult, err = ddb.dynamoSvc.BatchWriteItemWithContext(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: batchResult.UnprocessedItems,
			})
			if err != nil {
				return err
			}

			if len(batchResult.UnprocessedItems) == 0 {
				return nil
			}

		case <-timeout:
			// polling for table status has taken more than the timeout.
			return ErrDeleteTreeTimeout
		}
	}
}

type dynamodbLock struct {
	ddb      *Store
	last     *store.KVPair
	renewCh  chan struct{}
	unlockCh chan struct{}

	key   string
	value []byte
	ttl   time.Duration
}

func (l *dynamodbLock) Lock(ctx context.Context) (<-chan struct{}, error) {
	lockHeld := make(chan struct{})

	success, err := l.tryLock(ctx, lockHeld)
	if err != nil {
		return nil, err
	}
	if success {
		return lockHeld, nil
	}

	// TODO: This really needs a jitter for backoff.
	ticker := time.NewTicker(3 * time.Second)

	for {
		select {
		case <-ticker.C:
			success, err := l.tryLock(ctx, lockHeld)
			if err != nil {
				return nil, err
			}
			if success {
				return lockHeld, nil
			}
		case <-ctx.Done():
			return nil, ErrLockAcquireCancelled
		}
	}
}

func (l *dynamodbLock) Unlock(ctx context.Context) error {
	l.unlockCh <- struct{}{}

	_, err := l.ddb.AtomicDelete(ctx, l.key, l.last)
	if err != nil {
		return err
	}

	l.last = nil

	return nil
}

func (l *dynamodbLock) tryLock(ctx context.Context, lockHeld chan struct{}) (bool, error) {
	success, item, err := l.ddb.AtomicPut(ctx, l.key, l.value, l.last, &store.WriteOptions{TTL: l.ttl})
	if err != nil {
		if errors.Is(err, store.ErrKeyNotFound) || errors.Is(err, store.ErrKeyModified) || errors.Is(err, store.ErrKeyExists) {
			return false, nil
		}
		return false, err
	}
	if success {
		l.last = item
		// keep holding.
		go l.holdLock(ctx, lockHeld)
		return true, nil
	}

	return false, err
}

func (l *dynamodbLock) holdLock(ctx context.Context, lockHeld chan struct{}) {
	defer close(lockHeld)

	hold := func() error {
		_, item, err := l.ddb.AtomicPut(ctx, l.key, l.value, l.last, &store.WriteOptions{TTL: l.ttl})
		if err != nil {
			return err
		}

		l.last = item
		return nil
	}

	// may need a floor of 1 second set.
	heartbeat := time.NewTicker(l.ttl / 3)
	defer heartbeat.Stop()

	for {
		select {
		case <-heartbeat.C:
			if err := hold(); err != nil {
				return
			}
		case <-l.renewCh:
			return
		case <-l.unlockCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

func isItemExpired(item map[string]*dynamodb.AttributeValue) bool {
	v, ok := item[ttlAttribute]
	if !ok {
		return false
	}

	ttl, _ := strconv.ParseInt(aws.StringValue(v.N), 10, 64)
	return time.Unix(ttl, 0).Before(time.Now())
}

func decodeItem(item map[string]*dynamodb.AttributeValue) (*store.KVPair, error) {
	var key string
	if v, ok := item[partitionKey]; ok {
		key = aws.StringValue(v.S)
	}

	var revision int64
	if v, ok := item[revisionAttribute]; ok {
		var err error
		revision, err = strconv.ParseInt(aws.StringValue(v.N), 10, 64)
		if err != nil {
			return nil, err
		}
	}

	var encodedValue string
	if v, ok := item[encodedValueAttribute]; ok {
		encodedValue = aws.StringValue(v.S)
	}

	rawValue, err := base64.StdEncoding.DecodeString(encodedValue)
	if err != nil {
		return nil, err
	}

	return &store.KVPair{
		Key:       key,
		Value:     rawValue,
		LastIndex: uint64(revision),
	}, nil
}
