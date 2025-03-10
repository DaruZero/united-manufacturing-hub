//go:build kafka
// +build kafka

package internal

import (
	"fmt"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	jsoniter "github.com/json-iterator/go"
	"go.uber.org/zap"
	"syscall"

	"runtime"
	"runtime/debug"
	"time"
)

type KafkaKey struct {
	Putback *Putback `json:"Putback,omitempty"`
}

type Putback struct {
	FirstTsMS int64  `json:"FirstTsMs"`
	LastTsMS  int64  `json:"LastTsMs"`
	Amount    int64  `json:"Amount"`
	Reason    string `json:"Reason,omitempty"`
	Error     string `json:"Error,omitempty"`
}

type PutBackChanMsg struct {
	Msg               *kafka.Message
	Reason            string
	ErrorString       *string
	ForcePutbackTopic bool
}

// KafkaCommits is a counter for the number of commits done (to the db), this is used for stats only
var KafkaCommits = float64(0)

// KafkaMessages is a counter for the number of messages processed, this is used for stats only
var KafkaMessages = float64(0)

// KafkaPutBacks is a counter for the number of messages returned to kafka, this is used for stats only
var KafkaPutBacks = float64(0)

// KafkaConfirmed is a counter for the number of messages confirmed to kafka, this is used for stats only
var KafkaConfirmed = float64(0)

var ShuttingDownKafka bool
var ShutdownPutback bool
var nearMemoryLimit = false

func MemoryLimiter(allowedMemorySize int) {
	allowedSeventyFivePerc := uint64(float64(allowedMemorySize) * 0.9)
	allowedNintyPerc := uint64(float64(allowedMemorySize) * 0.75)
	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.Alloc > allowedNintyPerc {
			zap.S().Errorf("Memory usage is too high: %d bytes, slowing ingress !", m.TotalAlloc)
			nearMemoryLimit = true
			debug.FreeOSMemory()
			time.Sleep(FiveSeconds)
		}
		if m.Alloc > allowedSeventyFivePerc {
			zap.S().Errorf("Memory usage is high: %d bytes !", m.TotalAlloc)
			nearMemoryLimit = false
			runtime.GC()
			time.Sleep(FiveSeconds)
		} else {
			nearMemoryLimit = false
			time.Sleep(OneSecond)
		}
	}
}

// ProcessKafkaQueue processes the kafka queue and sends the messages to the processorChannel.
// It uses topic as regex for subscribing to kafka topics.
// If the putback channel is full, it will block until the channel is free.
func ProcessKafkaQueue(identifier string, topic string, processorChannel chan *kafka.Message, kafkaConsumer *kafka.Consumer, putBackChannel chan PutBackChanMsg, gracefulShutdown func()) {
	zap.S().Debugf("%s Starting Kafka consumer for topic %s", identifier, topic)
	err := kafkaConsumer.Subscribe(topic, nil)
	if err != nil {
		zap.S().Errorf("%s Failed to subscribe to topic %s: %s", identifier, topic, err)
		panic(err)
	}

	for !ShuttingDownKafka {
		if len(putBackChannel) > 100 {
			// We have too many CountMessagesToCommitLater in the put back channel, so we need to wait for some to be processed
			zap.S().Debugf("%s Waiting for put back channel to empty: %d", identifier, len(putBackChannel))
			time.Sleep(OneSecond)
			continue
		}

		if nearMemoryLimit {
			time.Sleep(OneSecond)
			continue
		}

		msg, isShuttingDown := waitNewMessages(identifier, kafkaConsumer, gracefulShutdown)
		if msg == nil {
			if isShuttingDown {
				return
			}
			continue
		}
		// Insert received message into the processor channel
		processorChannel <- msg
		// This is for stats only, it counts the number of messages received
		KafkaMessages += 1
	}
	zap.S().Debugf("%s Shutting down Kafka consumer for topic %s", identifier, topic)
}

// ProcessKafkaTopicProbeQueue processes the kafka queue and sends the messages to the processorChannel.
// It only subscribes to the topic used to announce a new topic.
func ProcessKafkaTopicProbeQueue(identifier string, processorChannel chan *kafka.Message, gracefulShutdown func()) {
	for !ShuttingDownKafka {
		msg, isShuttingDown := waitNewMessages(identifier, KafkaTopicProbeConsumer, gracefulShutdown)
		if msg == nil {
			if isShuttingDown {
				return
			}
			continue
		}
		// Insert received message into the processor channel
		processorChannel <- msg
		// This is for stats only, it counts the number of messages received
		KafkaMessages += 1
	}
	zap.S().Debugf("%s Shutting down Kafka Topic Probe consumer", identifier)
}

// waitNewMessages waits for new messages on the kafka consumer and checks for errors
func waitNewMessages(identifier string, kafkaConsumer *kafka.Consumer, gracefulShutdown func()) (msg *kafka.Message, isShuttingDown bool) {
	// Wait for new messages
	// This has a timeout, allowing ShuttingDownKafka to be checked
	msg, err := kafkaConsumer.ReadMessage(5000)
	if err != nil {
		switch err.(kafka.Error).Code() {
		// This is fine, and expected behaviour
		case kafka.ErrTimedOut:
			// Sleep to reduce CPU usage
			time.Sleep(OneSecond)
			return nil, false
		// This will occur when no topic for the regex is available !
		case kafka.ErrUnknownTopicOrPart:
			zap.S().Errorf("%s Unknown topic or partition: %s", identifier, err)
			gracefulShutdown()
			return nil, true
		default:
			zap.S().Warnf("%s Failed to read kafka message: %s: %s", identifier, err, err.(kafka.Error).Code())
			gracefulShutdown()
			return nil, true
		}
	}
	return msg, false
}

// StartPutbackProcessor starts the putback processor.
// It will put unprocessable messages back into the kafka queue, modifying there key to include the Reason and error.
func StartPutbackProcessor(identifier string, putBackChannel chan PutBackChanMsg, kafkaProducer *kafka.Producer, commitChannel chan *kafka.Message, putbackChanSize int) {
	zap.S().Debugf("%s Starting putback processor", identifier)
	// Loops until the shutdown signal is received and the channel is empty
	for !ShutdownPutback {
		select {
		case msgX := <-putBackChannel:
			{
				current := time.Now().UnixMilli()
				var msg = msgX.Msg
				var reason = msgX.Reason
				var errorString = msgX.ErrorString

				if msg == nil {
					continue
				}

				var msg2 kafka.Message
				if msg.Value != nil {
					msg2.Value = msg.Value
				}

				msg2.TopicPartition.Partition = 0

				topic := *msg.TopicPartition.Topic

				var rawKafkaKey []byte
				var putbackIndex = -1

				// Check for new header based putback info
				msg2.Headers = msg.Headers
				for i, header := range msg2.Headers {
					if header.Key == "putback" {
						rawKafkaKey = header.Value
						putbackIndex = i
						break
					}
				}

				var kafkaKey KafkaKey

				if rawKafkaKey == nil {
					kafkaKey = KafkaKey{
						&Putback{
							FirstTsMS: current,
							LastTsMS:  current,
							Amount:    1,
							Reason:    reason,
						},
					}
				} else {
					err := jsoniter.Unmarshal(rawKafkaKey, &kafkaKey)
					if err != nil {
						kafkaKey = KafkaKey{
							&Putback{
								FirstTsMS: current,
								LastTsMS:  current,
								Amount:    1,
								Reason:    reason,
							},
						}
					} else {
						kafkaKey.Putback.LastTsMS = current
						kafkaKey.Putback.Amount += 1
						kafkaKey.Putback.Reason = reason
						if msgX.ForcePutbackTopic || (kafkaKey.Putback.Amount >= 2 && kafkaKey.Putback.LastTsMS-kafkaKey.Putback.FirstTsMS > 300000) {
							topic = fmt.Sprintf("putback-error-%s", *msg.TopicPartition.Topic)

							if commitChannel != nil {
								commitChannel <- msg
							}
						}
					}
				}

				if errorString != nil && *errorString != "" {
					kafkaKey.Putback.Error = *errorString
				}

				var err error
				var header []byte
				header, err = jsoniter.Marshal(kafkaKey)
				if err != nil {
					zap.S().Errorf("%s Failed to marshal key: %v (%s)", identifier, kafkaKey, err)
					err = nil
				}
				if putbackIndex == -1 {
					msg2.Headers = append(msg.Headers, kafka.Header{
						Key:   "putback",
						Value: header,
					})
				} else {
					msg2.Headers[putbackIndex] = kafka.Header{
						Key:   "putback",
						Value: header,
					}
				}

				msgx := kafka.Message{
					TopicPartition: kafka.TopicPartition{
						Topic:     &topic,
						Partition: kafka.PartitionAny,
					},
					Value:   msg2.Value,
					Headers: msg2.Headers,
				}

				err = kafkaProducer.Produce(&msgx, nil)
				if err != nil {
					zap.S().Warnf("%s Failed to produce putback message: %s", identifier, err)
					// If the producer failed and the putback channel is full, use SIGINT to shut down !
					if len(putBackChannel) >= putbackChanSize {
						err = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
						if err != nil {
							zap.S().Errorf("%s Failed to send SIGINT to process: %s", identifier, err)
						}
					}
					putBackChannel <- PutBackChanMsg{&msgx, reason, errorString, false}
				}
				// This is for stats only and counts the amount of messages put back
				KafkaPutBacks += 1
				// Commit original message, after putback duplicate has been produced !
				commitChannel <- msg
			}
		}
	}
	zap.S().Infof("%s Putback processor shutting down", identifier)
}

// DrainChannel empties a channel into the high Throughput putback channel
func DrainChannel(identifier string, channelToDrain chan *kafka.Message, channelToDrainTo chan PutBackChanMsg, ShutdownChannel chan bool) bool {
	for len(channelToDrain) > 0 {
		select {
		case msg, ok := <-channelToDrain:
			if ok {
				channelToDrainTo <- PutBackChanMsg{msg, fmt.Sprintf("%s Shutting down", identifier), nil, false}
				KafkaPutBacks += 1
			} else {
				zap.S().Warnf("%s Channel to drain is closed", identifier)
				if ShutdownChannel != nil {
					ShutdownChannel <- false
				}
				return false
			}
		default:
			{
				zap.S().Debugf("%s Channel to drain is empty", identifier)
				if ShutdownChannel != nil {
					ShutdownChannel <- true
				}
				return true
			}
		}
	}
	zap.S().Debugf("%s channel drained", identifier)
	if ShutdownChannel != nil {
		ShutdownChannel <- true
	}
	return true
}

// DrainChannelSimple empties a channel into another channel
func DrainChannelSimple(channelToDrain chan *kafka.Message, channelToDrainTo chan PutBackChanMsg) bool {
	select {
	case msg, ok := <-channelToDrain:
		if ok {
			channelToDrainTo <- PutBackChanMsg{msg, "Shutting down", nil, false}
		} else {
			return false
		}
	default:
		{
			return true
		}
	}
	return false
}

// StartCommitProcessor starts the commit processor.
// It will commit messages to the kafka queue.
func StartCommitProcessor(identifier string, commitChannel chan *kafka.Message, kafkaConsumer *kafka.Consumer) {
	zap.S().Debugf("%s Starting commit processor", identifier)
	for !ShuttingDownKafka || len(commitChannel) > 0 {
		select {
		case msg := <-commitChannel:
			{
				_, err := kafkaConsumer.StoreMessage(msg)
				if err != nil {
					zap.S().Errorf("%s Error commiting %v: %s", identifier, msg, err)
					commitChannel <- msg
				} else {
					// This is for stats only, and counts the amounts of commits done to the kafka queue

					KafkaCommits += 1
				}
			}
		}
	}
	zap.S().Debugf("%s Stopped commit processor", identifier)
}

func StartEventHandler(identifier string, events chan kafka.Event, backChan chan PutBackChanMsg) {
	zap.S().Debugf("%s Starting event handler", identifier)
	for !ShuttingDownKafka || len(events) > 0 {
		select {
		case event := <-events:
			switch ev := event.(type) {
			case *kafka.Message:
				{
					if ev.TopicPartition.Error != nil {
						zap.S().Errorf("Error for %s: %v", identifier, ev.TopicPartition.Error)
						errS := ev.TopicPartition.Error.Error()
						if backChan != nil {
							backChan <- PutBackChanMsg{
								Msg:         ev,
								Reason:      "Event channel error",
								ErrorString: &errS,
							}
						}
					} else {
						// This is for stats only, and counts the amount of confirmed processed messages

						KafkaConfirmed += 1
					}
				}
			}
		default:
			time.Sleep(time.Millisecond * 100)
		}
	}
	zap.S().Debugf("%s Stopped event handler", identifier)
}
