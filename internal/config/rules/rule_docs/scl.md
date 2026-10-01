#### S7-SCL Review Principles
> Favor precision over recall: only raise an issue when you are confident it is a real defect that could stop the CPU or drive an output wrongly, and stay silent when the surrounding context is unclear. Assume TIA Portal S7-1200/1500 unless the diff shows S7-300/400 constructs (absolute `DB1.DBX0.0` addressing). Do not repeat compiler diagnostics. The `.scl` extension is also used by non-SCL formats; if the file is not IEC 61131-3 SCL (no `FUNCTION`, `FUNCTION_BLOCK`, `ORGANIZATION_BLOCK`, `DATA_BLOCK`, or `TYPE`), report nothing.

#### Obvious Typos or Spelling Errors
- Spelling errors in block names, tag names, UDT names, or `VAR` section member names at their declaration sites; do not report spelling errors at reference sites

#### Scan Cycle and Loop Safety
- An unbounded `WHILE` or `REPEAT`, or a very large `FOR`, inside a cyclic OB or a block it calls, risking a cycle-time overrun and watchdog stop
- A `FOR` counter or bound modified inside the loop body
- A `CASE` without `ELSE` where an unmatched value leaves outputs stale
- `EXIT` or `CONTINUE` skipping cleanup that the rest of the loop body requires

#### Arithmetic, Types and Comparisons
- `=` or `<>` on `REAL`/`LREAL` values where a tolerance comparison is needed
- Integer overflow or an implicit narrowing conversion (for example `DINT` to `INT`) on a value that can exceed the target range
- Division or `MOD` by a variable that can be zero
- `:=` and `=` confused in a condition or assignment
- A `STRING`/`WSTRING` value written into a target shorter than the source

#### Memory, Arrays and Block Interface
- An array index that can fall outside the declared bounds, which raises a programming error (OB121) and may stop the CPU on S7-300/400
- A `VAR_TEMP` variable read before it is written in the same call
- Edge-detection state (previous value, `R_TRIG` instance) kept in `VAR_TEMP` instead of `VAR`/static, so it is lost every cycle
- A `VAR_IN_OUT` parameter written while other code assumes the passed data is unchanged

#### Outputs and Safety Interlocks
- The same output tag assigned from more than one block or branch, so the last writer silently wins
- An output set with no reachable reset path
- Safety or emergency-stop logic bypassed by a newly added branch
- A manual or override flag that persists after a mode change
