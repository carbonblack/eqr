// Copyright (c) 2016 Twitch Interactive

package kinsumer

import (
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/sirupsen/logrus"
)

// Note: Not thread safe!

type checkpointer struct {
	shardID               string
	tableName             string
	dynamodb              dynamodbiface.DynamoDBAPI
	sequenceNumber        string
	ownerName             string
	ownerID               string
	maxAgeForClientRecord time.Duration
	captured              bool
	dirty                 bool
	mutex                 sync.Mutex
	finished              bool
	finalSequenceNumber   string
}

type checkpointRecord struct {
	Shard          string
	SequenceNumber *string // last read sequence number, null if the shard has never been consumed
	LastUpdate     int64   // timestamp of last commit/ownership change
	OwnerName      *string // uuid of owning client, null if the shard is unowned
	Finished       *int64  // timestamp of when the shard was fully consumed, null if it's active

	// Columns added to the table that are never used for decision making in the
	// library, rather they are useful for manual troubleshooting
	OwnerID       *string
	LastUpdateRFC string
	FinishedRFC   *string
}

// capture is a non-blocking call that attempts to capture the given shard/checkpoint.
// It returns a checkpointer on success, or nil if it fails to capture the checkpoint
func capture(
	shardID string,
	tableName string,
	dynamodbiface dynamodbiface.DynamoDBAPI,
	ownerName string,
	ownerID string,
	maxAgeForClientRecord time.Duration) (*checkpointer, error) {

	cutoff := time.Now().Add(-maxAgeForClientRecord).UnixNano()

	// Grab the entry from dynamo assuming there is one
	resp, err := dynamodbiface.GetItem(&dynamodb.GetItemInput{
		TableName:      aws.String(tableName),
		ConsistentRead: aws.Bool(true),
		Key: map[string]*dynamodb.AttributeValue{
			"Shard": {S: aws.String(shardID)},
		},
	})

	if err != nil {
		logger.WithFields(logrus.Fields{
		  "shardId": shardID,
		  "err": err.Error(),
		}).Error("error calling GetItem on shard checkpoint")
		return nil, fmt.Errorf("error calling GetItem on shard checkpoint: %v", err)
	}

	// Convert to struct so we can work with the values
	var record checkpointRecord
	if err = dynamodbattribute.UnmarshalMap(resp.Item, &record); err != nil {
		logger.WithFields(logrus.Fields{
		  "shardId": shardID,
		  "err": err.Error(),
		}).Error("error calling unmarshalling dynamo attribute in capture")
		return nil, err
	}

	// If the record is marked as owned by someone else, and has not expired
	if record.OwnerID != nil && record.LastUpdate > cutoff {
		// We fail to capture it
		return nil, nil
	}

	// Make sure the Shard is set in case there was no record
	record.Shard = shardID

	// Mark us as the owners
	record.OwnerID = &ownerID
	record.OwnerName = &ownerName

	// Update timestamp
	now := time.Now()
	record.LastUpdate = now.UnixNano()
	record.LastUpdateRFC = now.UTC().Format(time.RFC1123Z)

	item, err := dynamodbattribute.MarshalMap(record)
	if err != nil {
		logger.WithFields(logrus.Fields{
		  "shardId": shardID,
		  "err": err.Error(),
		}).Error("error calling marshalling dynamo attribute in capture")
		return nil, err
	}

	attrVals, err := dynamodbattribute.MarshalMap(map[string]interface{}{
		":cutoff":   aws.Int64(cutoff),
		":nullType": aws.String("NULL"),
	})
	if err != nil {
		logger.WithFields(logrus.Fields{
		  "shardId": shardID,
		  "err": err.Error(),
		}).Error("error calling marshalling dynamo attribute in capture")
		return nil, err
	}
	if _, err = dynamodbiface.PutItem(&dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
		// The OwnerID doesn't exist if the entry doesn't exist, but PutItem with a marshaled
		// checkpointRecord sets a nil OwnerID to the NULL type.
		ConditionExpression: aws.String(
			"attribute_not_exists(OwnerID) OR attribute_type(OwnerID, :nullType) OR LastUpdate <= :cutoff"),
		ExpressionAttributeValues: attrVals,
	}); err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == "ConditionalCheckFailedException" {
			// We failed to capture it
			return nil, nil
		}
		logger.WithFields(logrus.Fields{
		  "shardId": shardID,
		  "err": err.Error(),
		}).Error("error putting item to dynamo")
		return nil, err
	}

	checkpointer := &checkpointer{
		shardID:               shardID,
		tableName:             tableName,
		dynamodb:              dynamodbiface,
		ownerName:             ownerName,
		ownerID:               ownerID,
		sequenceNumber:        aws.StringValue(record.SequenceNumber),
		maxAgeForClientRecord: maxAgeForClientRecord,
		captured:              true,
	}

	logger.WithFields(logrus.Fields{
	    "sequenceNumber": record.SequenceNumber,
		"shardId": shardID,
	    "ownerId": ownerID,
	}).Debug("Checkpoint captured")

	return checkpointer, nil
}

// commit writes the latest SequenceNumber consumed to dynamo and updates LastUpdate.
// Returns true if we set Finished in dynamo because the library user finished consuming the shard.
// Once that has happened, the checkpointer should be released and never grabbed again.
func (cp *checkpointer) commit() (bool, error) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()
	if !cp.dirty && !cp.finished {
		return false, nil
	}
	now := time.Now()


	sn := &cp.sequenceNumber
	if cp.sequenceNumber == "" {
		// We are not allowed to pass empty strings to dynamo, so instead pass a nil *string
		// to 'unset' it
		sn = nil
	}

	record := checkpointRecord{
		Shard:          cp.shardID,
		SequenceNumber: sn,
		LastUpdate:     now.UnixNano(),
		LastUpdateRFC:  now.UTC().Format(time.RFC1123Z),
	}
	finished := false
	if cp.finished && (cp.sequenceNumber == cp.finalSequenceNumber || cp.finalSequenceNumber == "") {
		record.Finished = aws.Int64(now.UnixNano())
		record.FinishedRFC = aws.String(now.UTC().Format(time.RFC1123Z))
		finished = true
	}
	record.OwnerID = &cp.ownerID
	record.OwnerName = &cp.ownerName

	item, err := dynamodbattribute.MarshalMap(&record)
	if err != nil {
		logger.WithFields(logrus.Fields{
		  "sequenceNumber": sn,
		  "shardId": cp.shardID,
		  "err": err.Error(),
		}).Error("error calling marshalling dynamo attribute in commit")
		return false, err
	}

	attrVals, err := dynamodbattribute.MarshalMap(map[string]interface{}{
		":ownerID": aws.String(cp.ownerID),
	})
	if err != nil {
		logger.WithFields(logrus.Fields{
		  "sequenceNumber": sn,
		  "shardId": cp.shardID,
		  "err": err.Error(),
		}).Error("error calling marshalling dynamo attribute in commit")
		return false, err
	}
	if _, err = cp.dynamodb.PutItem(&dynamodb.PutItemInput{
		TableName:                 aws.String(cp.tableName),
		Item:                      item,
		ConditionExpression:       aws.String("OwnerID = :ownerID"),
		ExpressionAttributeValues: attrVals,
	}); err != nil {
		logger.WithFields(logrus.Fields{
		  "sequenceNumber": sn,
		  "shardId": cp.shardID,
		  "ownerId": cp.ownerID,
		  "err": err.Error(),
		}).Error("error committing checkpoint")
		return false, fmt.Errorf("error committing checkpoint: %s", err)
	}

	logger.WithFields(logrus.Fields{
	    "sequenceNumber": sn,
		"shardId": cp.shardID,
	}).Debug("Checkpoint committed")

	cp.dirty = false
	return finished, nil
}

// release releases our ownership of the checkpoint in dynamo so another client can take it
func (cp *checkpointer) release() error {
	now := time.Now()

	attrVals, err := dynamodbattribute.MarshalMap(map[string]interface{}{
		":ownerID":        aws.String(cp.ownerID),
		":sequenceNumber": aws.String(cp.sequenceNumber),
		":lastUpdate":     aws.Int64(now.UnixNano()),
		":lastUpdateRFC":  aws.String(now.UTC().Format(time.RFC1123Z)),
	})
	if err != nil {
		logger.WithFields(logrus.Fields{
		  "sequenceNumber": cp.sequenceNumber,
		  "shardId": cp.shardID,
		  "err": err.Error(),
		}).Error("error calling marshalling dynamo attribute in release")
		return err
	}
	if _, err = cp.dynamodb.UpdateItem(&dynamodb.UpdateItemInput{
		TableName: aws.String(cp.tableName),
		Key: map[string]*dynamodb.AttributeValue{
			"Shard": {S: aws.String(cp.shardID)},
		},
		UpdateExpression: aws.String("REMOVE OwnerID, OwnerName " +
			"SET LastUpdate = :lastUpdate, LastUpdateRFC = :lastUpdateRFC, " +
			"SequenceNumber = :sequenceNumber"),
		ConditionExpression:       aws.String("OwnerID = :ownerID"),
		ExpressionAttributeValues: attrVals,
	}); err != nil {
		logger.WithFields(logrus.Fields{
		  "sequenceNumber": cp.sequenceNumber,
		  "shardId": cp.shardID,
		  "err": err.Error(),
		}).Error("error releasing checkpoint")
		return fmt.Errorf("error releasing checkpoint: %s", err)
	}

	cp.captured = false

	logger.WithFields(logrus.Fields{
	    "sequenceNumber": cp.sequenceNumber,
		"shardId": cp.shardID,
	}).Debug("Checkpoint released")

	return nil
}

// update updates the current sequenceNumber of the checkpoint, marking it dirty if necessary
func (cp *checkpointer) update(seqNum string) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()
	cp.dirty = cp.dirty || cp.sequenceNumber != seqNum
	cp.sequenceNumber = seqNum

	logger.WithFields(logrus.Fields{
	    "sequenceNumber": cp.sequenceNumber,
		"shardId": cp.shardID,
	}).Debug("Checkpoint updated")

	logger.WithFields(logrus.Fields{
	    "sequenceNumber": cp.sequenceNumber,
		"shardId": cp.shardID,
	}).Debug("Reference count channel closed")
}

// finish marks the given sequence number as the final one for the shard.
// sequenceNumber is the empty string if we never read anything from the shard.
func (cp *checkpointer) finish(sequenceNumber string) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()
	cp.finalSequenceNumber = sequenceNumber
	cp.finished = true

	logger.WithFields(logrus.Fields{
	    "sequenceNumber": sequenceNumber,
		"shardId": cp.shardID,
	}).Debug("Checkpoint finished")
}

// loadCheckpoints returns checkpoint records from dynamo mapped by shard id.
func loadCheckpoints(db dynamodbiface.DynamoDBAPI, tableName string) (map[string]*checkpointRecord, error) {
	params := &dynamodb.ScanInput{
		TableName:      aws.String(tableName),
		ConsistentRead: aws.Bool(true),
	}

	var records []*checkpointRecord
	var innerError error
	err := db.ScanPages(params, func(p *dynamodb.ScanOutput, lastPage bool) (shouldContinue bool) {
		for _, item := range p.Items {
			var record checkpointRecord
			innerError = dynamodbattribute.UnmarshalMap(item, &record)
			if innerError != nil {
				return false
			}
			records = append(records, &record)
		}

		return !lastPage
	})

	if innerError != nil {
		logger.WithFields(logrus.Fields{
		  "table": tableName,
		  "err": err.Error(),
		}).Error("error calling unmarshalling dynamo attribute in loadCheckpoints")
		return nil, innerError
	}

	if err != nil {
		logger.WithFields(logrus.Fields{
		  "table": tableName,
		  "err": err.Error(),
		}).Error("error scanning dynamo")
		return nil, err
	}

	checkpointMap := make(map[string]*checkpointRecord, len(records))
	for _, checkpoint := range records {
		checkpointMap[checkpoint.Shard] = checkpoint
	}

	logger.WithFields(logrus.Fields{
	    "table": tableName,
		"checkpoints": checkpointMap,
	}).Debug("Checkpoints loaded")

	return checkpointMap, nil
}
