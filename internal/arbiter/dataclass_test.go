package arbiter

import "testing"

func TestClassifyTags(t *testing.T) {
	tests := []struct {
		name    string
		tags    []string
		want    DataClass
		wantErr bool
	}{
		{name: "untagged defaults to personal", tags: nil, want: DataClassPersonal},
		{name: "empty slice defaults to personal", tags: []string{}, want: DataClassPersonal},
		{name: "unrecognized project tag defaults to personal", tags: []string{"project:aida"}, want: DataClassPersonal},
		{name: "explicit personal", tags: []string{"personal"}, want: DataClassPersonal},
		{name: "finances tag", tags: []string{"finances"}, want: DataClassPersonal},
		{name: "project:finances tag", tags: []string{"project:finances"}, want: DataClassPersonal},
		{name: "project:life-log tag", tags: []string{"project:life-log"}, want: DataClassPersonal},
		{name: "family tag", tags: []string{"family"}, want: DataClassPersonal},
		{name: "career tag", tags: []string{"career"}, want: DataClassPersonal},
		{name: "project:butterstack tag", tags: []string{"project:butterstack"}, want: DataClassButterstack},
		{name: "butterstack tag", tags: []string{"butterstack"}, want: DataClassButterstack},
		{name: "cta tag", tags: []string{"cta"}, want: DataClassButterstack},
		{name: "project:cta tag", tags: []string{"project:cta"}, want: DataClassButterstack},
		{name: "its tag", tags: []string{"its"}, want: DataClassButterstack},
		{name: "games tag", tags: []string{"games"}, want: DataClassGames},
		{name: "game tag", tags: []string{"game"}, want: DataClassGames},
		{name: "project:plt tag", tags: []string{"project:plt"}, want: DataClassGames},
		{name: "bsg studio tag is games", tags: []string{"bsg", "profile:home"}, want: DataClassGames},
		{name: "project:butterup is games", tags: []string{"project:butterup"}, want: DataClassGames},
		{name: "retired project:butter_stack variant is butterstack", tags: []string{"project:butter_stack", "ops"}, want: DataClassButterstack},
		{name: "bsg under the butterstack company tag stays butterstack", tags: []string{"project:butterstack", "bsg"}, want: DataClassButterstack},
		{name: "project:tb tag", tags: []string{"project:tb"}, want: DataClassGames},
		{name: "project:bu tag", tags: []string{"project:bu"}, want: DataClassGames},
		{name: "project:butter-up tag", tags: []string{"project:butter-up"}, want: DataClassGames},
		{name: "project:pilot-light tag", tags: []string{"project:pilot-light"}, want: DataClassGames},
		{name: "public tag", tags: []string{"public"}, want: DataClassPublic},
		{name: "mixed games and personal, personal wins", tags: []string{"games", "personal"}, want: DataClassPersonal},
		{name: "mixed games and public, games wins", tags: []string{"public", "games"}, want: DataClassGames},
		{name: "mixed butterstack and public, butterstack wins", tags: []string{"public", "butterstack"}, want: DataClassButterstack},
		{name: "case-insensitive", tags: []string{"BUTTERSTACK"}, want: DataClassButterstack},
		{name: "whitespace tolerant", tags: []string{"  personal  "}, want: DataClassPersonal},
		{name: "butterstack and personal together is an error", tags: []string{"butterstack", "personal"}, wantErr: true},
		{name: "butterstack and project:finances together is an error", tags: []string{"project:butterstack", "project:finances"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyTags(tt.tags)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ClassifyTags(%v) = %v, nil; want an error", tt.tags, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClassifyTags(%v) unexpected error: %v", tt.tags, err)
			}
			if got != tt.want {
				t.Errorf("ClassifyTags(%v) = %q, want %q", tt.tags, got, tt.want)
			}
		})
	}
}
