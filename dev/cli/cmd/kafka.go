package cmd

import (
	"github.com/spf13/viper"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message/infrastructure/kafka"
	"github.com/spf13/cobra"
)

// kafkaCmd is a mid-level command for working with the kafka pub/sub provider.
var kafkaCmd = &cobra.Command{
	Use:   "kafka",
	Short: "Consume or produce messages from the kafka pub/sub provider",
	Long: `Consume or produce messages from the kafka pub/sub provider.

For the configuration of consuming/producing of the message, check the help of the relevant command.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		err := rootCmd.PersistentPreRunE(cmd, args)
		if err != nil {
			return err
		}
		logger.Debug("Using kafka pub/sub", watermill.LogFields{})

		brokers := viper.GetStringSlice("kafka.brokers")

		producer, err = kafka.NewPublisher(brokers, kafka.DefaultMarshaler{}, nil, logger)
		if err != nil {
			return err
		}

		consumer, err = kafka.NewSubscriber(kafka.SubscriberConfig{
			Brokers: brokers,
		}, nil, kafka.DefaultMarshaler{}, logger)
		if err != nil {
			return err
		}

		return nil
	},
}

func init() {
	// Here you will define your flags and configuration settings.

	rootCmd.AddCommand(kafkaCmd)
	kafkaCmd.AddCommand(consumeCmd)
	kafkaCmd.AddCommand(produceCmd)

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	kafkaCmd.PersistentFlags().StringSlice("kafka.brokers", nil, "A list of kafka brokers")
	if err := kafkaCmd.MarkPersistentFlagRequired("kafka.brokers"); err != nil {
		panic(err)
	}
	if err := viper.BindPFlag("kafka.brokers", kafkaCmd.PersistentFlags().Lookup("kafka.brokers")); err != nil {
		panic(err)
	}

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// produceCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
