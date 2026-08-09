# CLAUDE.md

- W komunikacji user-facing (wiadomości bota, strona, maile) używaj form neutralnych płciowo. Zamiast "zapisany" pisz np. "twój zapis jest aktywny", zamiast "on/ona zrobił(a)" przeformułuj zdanie. Tam gdzie się nie da, dopuszczalna forma z ukośnikiem ("nieobecny/a").
- Dla nowego kodu wymagany jest **system test, który dowodzi, że feature działa poprawnie**. Nie wystarczy unit test na wycinku logiki: test ma przejść przez realny przepływ funkcji z perspektywy użytkownika (wzorzec: `test/e2e/` przeciw prawdziwemu Synapse dla E2EE bota, `TestRegisterFlowBotToWeb` dla łańcucha bot->web). Feature bez takiego testu jest niedokończony.
