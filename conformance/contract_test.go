// 套件自证：正对照全绿 + 负对照判红（fake transport 语义见 fake.go）。
// 判红与判绿走同一组检查（同一单源）——负对照不是另写一套断言。
package conformance

import "testing"

// TestConformanceCompliantTransport 正对照：Run 对合规 fake transport 充许全绿。
func TestConformanceCompliantTransport(t *testing.T) {
	Run(t, NewCompliantContract())
}

// TestNegativeControlViolatingTransport 负对照：违规 fake transport 必须被判红。
//
// 违规 fake 带两处反模式（按文案猜分类 + 剥信封），套件凡走被测工具面的
// 语义检查都必须对它返回违规；纯 iface 语义（能力接口零值/缺省）不经 fake，
// 正负对照同绿——那是刻意的：它们钉的是 iface 消费口径，与工具面无关。
func TestNegativeControlViolatingTransport(t *testing.T) {
	c := NewViolatingContract()

	mustFlag := []struct {
		name  string
		check func(t *testing.T, c Contract) error
	}{
		{"errkind_unreachable_shape", checkUnreachable},
		{"errkind_protocol_shape", checkProtocol},
		{"errkind_toolmissing_shape", checkToolMissing},
		{"errkind_invalidargs_shape", checkInvalidArgs},
		{"errkind_unknown_business_failure", checkUnknown},
		{"no_retry_toolmissing_invalidargs", checkNoRetryPermanent},
		{"no_retry_unknown_conservative", checkNoRetryUnknown},
		{"envelope_meta_not_stripped", checkEnvelope},
	}
	for _, tc := range mustFlag {
		if err := tc.check(t, c); err == nil {
			t.Errorf("负对照失效：%s 对违规实现竟然放行", tc.name)
		}
	}

	for _, tc := range []struct {
		name  string
		check func(t *testing.T, c Contract) error
	}{
		{"rawnamer_zero_default", checkRawNamer},
		{"outputschemer_zero_default", checkOutputSchemer},
	} {
		if err := tc.check(t, c); err != nil {
			t.Errorf("%s 是纯 iface 语义（不经 fake），不应受违规实现影响：%v", tc.name, err)
		}
	}
}
