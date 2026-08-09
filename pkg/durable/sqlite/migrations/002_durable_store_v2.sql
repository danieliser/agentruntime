DROP TRIGGER generations_identity_immutable;

-- A provider-native session/thread ID is commonly learned from the first
-- runtime record after generation creation. Permit that one-way discovery,
-- while retaining immutability after a non-empty identity has been recorded.
CREATE TRIGGER generations_identity_immutable
BEFORE UPDATE ON runtime_generations
WHEN NEW.session_id <> OLD.session_id
  OR NEW.generation <> OLD.generation
  OR NEW.runtime <> OLD.runtime
  OR NEW.container_id <> OLD.container_id
  OR NEW.image_reference <> OLD.image_reference
  OR NEW.image_digest <> OLD.image_digest
  OR NEW.sandbox_profile <> OLD.sandbox_profile
  OR (
      NEW.provider_id <> OLD.provider_id
      AND NOT (OLD.provider_id = '' AND NEW.provider_id <> '')
  )
  OR NEW.docker_log_driver <> OLD.docker_log_driver
  OR NEW.docker_log_options_json <> OLD.docker_log_options_json
  OR NEW.created_at_ns <> OLD.created_at_ns
BEGIN
    SELECT RAISE(ABORT, 'generation identity is immutable');
END;
