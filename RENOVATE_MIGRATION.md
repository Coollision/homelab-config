# Renovate Configuration Migration

This document explains the migration of the Renovate configuration from deprecated options to modern equivalents.

## Changes Made

### Deprecated Presets Removed
- `:pinDevDependencies` → Replaced with `"pinDigests": true`
- `:prHourlyLimitNone` → Replaced with `"prHourlyLimit": 0`
- `:rebaseStalePrs` → Replaced with `"rebaseWhen": "conflicted"`

### Kubernetes Manager Configuration
- `managerFilePatterns` → Changed to `fileMatch` (modern syntax)
- Regex pattern `/\\.yaml$/` → Simplified to `\\.yaml$`

### Automerge Configuration  
- `automergeType: "branch"` → Removed (deprecated)
- Added `"platformAutomerge": true` for modern automerge behavior
- Schedule format updated from cron-like to human-readable format

### Schedule Format Updates
- `"* 1-4 * * *"` → `"after 1am and before 4am"`
- `"* 19-22 * * *"` → `"after 7pm and before 10pm"`

## Benefits

1. **Elimination of Migration Warnings**: Removes all deprecated configuration options
2. **Modern Schedule Format**: Human-readable schedule expressions
3. **Better Automerge Behavior**: Uses platform-native automerge capabilities
4. **Simplified Regex Patterns**: Cleaner YAML file matching

## Configuration Overview

The updated configuration:
- Maintains the same functional behavior as before
- Uses modern Renovate syntax and presets
- Continues to manage Helm charts and Kubernetes YAML files
- Preserves automerge settings for patch updates
- Keeps the same timezone and schedule preferences