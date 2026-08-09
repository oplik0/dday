# CLAUDE.md

- Dla nowego kodu wymagany jest **system test, który dowodzi, że feature działa poprawnie**. Nie wystarczy unit test na wycinku logiki: test ma przejść przez realny przepływ funkcji z perspektywy użytkownika (wzorzec: `test/e2e/` przeciw prawdziwemu Synapse dla E2EE bota, `TestRegisterFlowBotToWeb` dla łańcucha bot->web). Feature bez takiego testu jest niedokończony.
