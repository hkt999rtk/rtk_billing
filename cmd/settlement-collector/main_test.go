package main

import "testing"

func TestLoadSettings(t *testing.T) {
	values := map[string]string{
		"DATABASE_URL":                            "postgres://billing.invalid/billing",
		"MQTT_USAGE_SETTLEMENT_BASE_URL":          "http://mqttusage.video-cloud.svc.cluster.local:8082",
		"MQTT_USAGE_SETTLEMENT_TOKEN":             "0123456789abcdef0123456789abcdef",
		"BILLING_SETTLEMENT_COLLECTOR_INTERVAL":   "3s",
		"BILLING_SETTLEMENT_COLLECTOR_BATCH_SIZE": "25",
	}
	cfg, err := loadSettings(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.interval.String() != "3s" || cfg.batchSize != 25 {
		t.Fatalf("unexpected settings: %+v", cfg)
	}
}

func TestLoadSettingsRejectsUnsafeOrUnboundedValues(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL":                   "postgres://billing.invalid/billing",
		"MQTT_USAGE_SETTLEMENT_BASE_URL": "http://mqttusage.video-cloud.svc.cluster.local:8082",
		"MQTT_USAGE_SETTLEMENT_TOKEN":    "0123456789abcdef0123456789abcdef",
	}
	for name, override := range map[string]map[string]string{
		"short token": {"MQTT_USAGE_SETTLEMENT_TOKEN": "short"},
		"fast loop":   {"BILLING_SETTLEMENT_COLLECTOR_INTERVAL": "1ms"},
		"large batch": {"BILLING_SETTLEMENT_COLLECTOR_BATCH_SIZE": "501"},
	} {
		t.Run(name, func(t *testing.T) {
			values := make(map[string]string, len(base)+1)
			for key, value := range base {
				values[key] = value
			}
			for key, value := range override {
				values[key] = value
			}
			if _, err := loadSettings(func(key string) string { return values[key] }); err == nil {
				t.Fatal("expected invalid settings")
			}
		})
	}
}
