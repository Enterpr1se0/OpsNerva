package service

import (
	"context"
	"fmt"
	"io"

	"eino-ops-agent/internal/skills"
)

type SkillContentInput struct {
	Content string `json:"content"`
}

func (s *Service) InitializeSkills() error {
	_, err := s.ListSkills()
	return err
}

func (s *Service) ListSkills() ([]skills.Skill, error) {
	if s.skills == nil {
		return []skills.Skill{}, nil
	}
	return s.skills.List()
}

func (s *Service) ListEnabledSkills() ([]skills.Skill, error) {
	items, err := s.ListSkills()
	if err != nil {
		return nil, err
	}
	result := make([]skills.Skill, 0, len(items))
	for _, item := range items {
		if item.Enabled {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *Service) GetAdminSkill(name string) (skills.Skill, error) {
	if s.skills == nil {
		return skills.Skill{}, fmt.Errorf("%w: %s", skills.ErrNotFound, name)
	}
	return s.skills.Get(name)
}

func (s *Service) LoadSkill(ctx context.Context, name, actor string) (skills.Skill, error) {
	skill, err := s.GetAdminSkill(name)
	if err != nil {
		return skills.Skill{}, err
	}
	if !skill.Enabled {
		return skills.Skill{}, fmt.Errorf("%w: %s", skills.ErrDisabled, skill.Name)
	}
	s.audit(ctx, "", "skill_loaded", actor, map[string]any{
		"skill_name": skill.Name, "content_sha256": skill.ContentSHA256, "session_id": SessionIDFromContext(ctx),
	})
	return skill, nil
}

func (s *Service) SetAdminSkillEnabled(ctx context.Context, name string, enabled bool, actor string) (skills.Skill, error) {
	if s.skills == nil {
		return skills.Skill{}, fmt.Errorf("skill registry is not configured")
	}
	skill, err := s.skills.SetEnabled(name, enabled)
	if err != nil {
		return skills.Skill{}, err
	}
	eventType := "skill_disabled"
	if enabled {
		eventType = "skill_enabled"
	}
	s.audit(ctx, "", eventType, actor, map[string]any{"skill_name": skill.Name, "content_sha256": skill.ContentSHA256})
	return skill, nil
}

func (s *Service) SaveAdminSkill(ctx context.Context, name, content, actor string) (skills.Skill, error) {
	if s.skills == nil {
		return skills.Skill{}, fmt.Errorf("skill registry is not configured")
	}
	skill, err := s.skills.Save(name, content)
	if err != nil {
		return skills.Skill{}, err
	}
	s.audit(ctx, "", "skill_saved", actor, map[string]any{
		"skill_name": skill.Name, "content_sha256": skill.ContentSHA256, "size_bytes": skill.SizeBytes,
	})
	return skill, nil
}

func (s *Service) ImportAdminSkills(ctx context.Context, name, filename string, source io.Reader, actor string) ([]skills.Skill, error) {
	if s.skills == nil {
		return nil, fmt.Errorf("skill registry is not configured")
	}
	imported, err := s.skills.ImportPackage(name, filename, source)
	if err != nil {
		return nil, err
	}
	for _, skill := range imported {
		s.audit(ctx, "", "skill_uploaded", actor, map[string]any{
			"skill_name": skill.Name, "content_sha256": skill.ContentSHA256, "file_count": skill.FileCount, "size_bytes": skill.SizeBytes,
		})
	}
	return imported, nil
}

func (s *Service) DeleteAdminSkill(ctx context.Context, name, actor string) error {
	if s.skills == nil {
		return fmt.Errorf("skill registry is not configured")
	}
	skill, err := s.skills.Get(name)
	if err != nil {
		return err
	}
	if err := s.skills.Delete(name); err != nil {
		return err
	}
	s.audit(ctx, "", "skill_deleted", actor, map[string]any{"skill_name": skill.Name, "content_sha256": skill.ContentSHA256})
	return nil
}
