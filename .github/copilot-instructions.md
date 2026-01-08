
## Commit Message Format

Please follow the **Conventional Commits** specification for all commit messages:

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

### Types
- **feat**: A new feature
- **fix**: A bug fix
- **docs**: Documentation only changes
- **style**: Changes that do not affect the meaning of the code (white-space, formatting, missing semi-colons, etc)
- **refactor**: A code change that neither fixes a bug nor adds a feature
- **perf**: A code change that improves performance
- **test**: Adding missing tests or correcting existing tests
- **build**: Changes that affect the build system or external dependencies
- **ci**: Changes to CI configuration files and scripts
- **chore**: Other changes that don't modify src or test files
- **types**: Changes to TypeScript type definitions or generation

### Scopes (when applicable)
- **models**: Changes to Go model structs in `pkg/models/`
- **api**: Changes to API response structures in `pkg/api/`
- **types**: TypeScript type generation or definitions
- **scripts**: Build/generation scripts
- **docs**: Documentation updates

### Examples

**Good commit messages:**
```
feat(models): add sprite interval field to Media struct

fix(api): correct JSON tags for ErrorResponse message field

docs(readme): update TypeScript generation workflow

refactor(models): standardize BSON tag formatting across all structs

types: regenerate TypeScript definitions from updated Go models

chore: update Go dependencies to latest versions
```

**Bad commit messages:**
```
Update media.go
Fix bug
Add new field
WIP
.
```

## Code Style Guidelines

### Go Code
- Follow standard Go conventions (gofmt, golint)
- Use meaningful struct field comments
- Include both `json` and `bson` tags with `omitempty`
- Use consistent field naming (PascalCase for exports)
- Add Swagger annotations for API documentation

### TypeScript Generation
- Always run `npm run generate` after modifying Go models
- Ensure new models are referenced in `cmd/main.go`
- Test generated TypeScript types before committing

### Documentation
- Update README.md when adding new models or changing workflows
- Include usage examples for new features
- Document any breaking changes in commit body

## Branch Naming
Use descriptive branch names with prefixes:
- `feat/` for new features
- `fix/` for bug fixes  
- `refactor/` for refactoring
- `docs/` for documentation updates
- `chore/` for maintenance tasks

Examples: `feat/user-authentication-model`, `fix/media-duration-validation`, `refactor/media-model`