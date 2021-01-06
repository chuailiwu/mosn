package router

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"mosn.io/api"
	v2 "mosn.io/mosn/pkg/config/v2"
	"mosn.io/mosn/pkg/protocol"
	"mosn.io/mosn/pkg/variable"
)

func TestDslExpressionRouteRuleImpl_Match(t *testing.T) {
	virtualHostImpl := &VirtualHostImpl{virtualHostName: "test"}
	testCases := []struct {
		names    []string
		values   []string
		headers  api.HeaderMap
		expected bool
	}{
		{[]string{protocol.MosnHeaderMethod, protocol.MosnHeaderHostKey}, []string{"method1", "test1"}, protocol.CommonHeader{"a1": "b1"}, true},
		{[]string{protocol.MosnHeaderMethod, protocol.MosnHeaderHostKey}, []string{"method1", "test2"}, protocol.CommonHeader{"a1": "b1"}, false},
		{[]string{protocol.MosnHeaderMethod, protocol.MosnHeaderHostKey}, []string{"method1", "test2"}, protocol.CommonHeader{"a1": "b2"}, false},
		{[]string{protocol.MosnHeaderMethod, protocol.MosnHeaderHostKey}, []string{"method1", "test1"}, nil, false},
		{[]string{protocol.MosnHeaderMethod, protocol.MosnHeaderHostKey}, []string{"method1", "test1"}, protocol.CommonHeader{"a2": "b2"}, false},
	}
	// header == test || (method == test && regex.MatchString(uri)) || uri == /1234
	route := &v2.Router{
		RouterConfig: v2.RouterConfig{
			Match: v2.RouterMatch{
				DslExpressions: []v2.DslExpressionMatcher{
					{
						Expression: "conditional((request.method == \"method1\") && (request.host == \"test1\"),true,false)",
					},
					{
						Expression: "conditional((request.headers[\"a1\"] == \"b1\"),true,false)",
					},
				},
			},
			Route: v2.RouteAction{
				RouterActionConfig: v2.RouterActionConfig{
					ClusterName: "test",
				},
			},
		},
	}
	base, _ := NewRouteRuleImplBase(virtualHostImpl, route)
	rr := &DslExpressionRouteRuleImpl{base, parseConfigToDslExpression(route.Match.DslExpressions), route.Match.DslExpressions}

	for i, tc := range testCases {
		ctx := variable.NewVariableContext(context.Background())
		for i := 0; i < len(tc.names); i++ {
			variable.SetVariableValue(ctx, tc.names[i], tc.values[i])
		}
		result := rr.Match(ctx, tc.headers)
		assert.EqualValuesf(t, result != nil, tc.expected, "#%d want matched %v, but get matched %v\n", i, tc.expected, result != nil)
		if result != nil {
			assert.EqualValuesf(t, api.Variable, result.RouteRule().PathMatchCriterion().MatchType(), "#%d match type is not expected", i)
		}
	}
}
