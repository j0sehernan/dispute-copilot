package domain

type Recommendation string

const (
	RecommendFight  Recommendation = "fight"
	RecommendAccept Recommendation = "accept"
)

func (r Recommendation) Valid() bool { return r == RecommendFight || r == RecommendAccept }

type RequirementStatus string

const (
	RequirementMet             RequirementStatus = "met"
	RequirementNotMet          RequirementStatus = "not_met"
	RequirementMissingEvidence RequirementStatus = "missing_evidence"
)

type RequirementResult struct {
	ID          string            `json:"id"`
	Text        string            `json:"text"`
	Status      RequirementStatus `json:"status"`
	Evidence    []string          `json:"evidence"`
	Explanation string            `json:"explanation"`
}

type Reason struct {
	Text     string   `json:"text"`
	Evidence []string `json:"evidence"`
	Rules    []string `json:"rules"`
}

// Analysis is the agent's assessment of a dispute. Recommendation is always
// the one implied by the rule policy; ModelRecommendation keeps what the
// model said, so a disagreement is visible to the reviewer.
type Analysis struct {
	RuleID              string              `json:"rule_id"`
	RuleMatch           string              `json:"rule_match"`
	Requirements        []RequirementResult `json:"requirements"`
	Recommendation      Recommendation      `json:"recommendation"`
	ModelRecommendation Recommendation      `json:"model_recommendation"`
	PolicyOverride      bool                `json:"policy_override"`
	Reasons             []Reason            `json:"reasons"`
	MissingEvidence     []string            `json:"missing_evidence"`
	RuleChunks          []string            `json:"rule_chunks"`
}

// Policy describes when a dispute is worth fighting: any one of the groups
// must have all of its requirements met.
type Policy struct {
	FightWhen [][]string
}

func (p Policy) Decide(results []RequirementResult) Recommendation {
	met := make(map[string]bool, len(results))
	for _, r := range results {
		met[r.ID] = r.Status == RequirementMet
	}
	for _, group := range p.FightWhen {
		ok := len(group) > 0
		for _, id := range group {
			ok = ok && met[id]
		}
		if ok {
			return RecommendFight
		}
	}
	return RecommendAccept
}
