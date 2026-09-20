# bot

Petit harnais LLM en Go, sans dépendance externe (stdlib uniquement).

- Parle à un LLM via une API compatible OpenAI (`/v1/chat/completions`), typiquement un serveur local (llama.cpp, vLLM, Ollama en mode OpenAI, etc.).
- Mode `chat` : session interactive en console, avec streaming de la réponse.
- Mode `mail` : lit périodiquement les mails non lus d'une boîte IMAP, génère une réponse via le LLM et l'envoie automatiquement par SMTP, avec le même sujet que l'original (sans préfixe "Re:"). Le threading ("affichage conversation" de Thunderbird et consorts) repose sur les en-têtes `Message-Id`/`In-Reply-To`/`References`, générés et chaînés correctement.
- Gère l'historique de conversation et le **compacte automatiquement** (résumé via le LLM) quand le contexte estimé atteint ~90% de la fenêtre du modèle.
- Tool calling (function calling) : le LLM peut appeler des outils locaux (shell, lecture/écriture de fichier, requête HTTP). Les appels d'outils et leurs résultats sont affichés/journalisés à part, nettement séparés du texte de réponse.

## Configuration

Le fichier `.env` est lu depuis le **workspace** (`WORKSPACE_DIR/.env`, `~/bot-workspace/.env` par défaut) — **jamais** depuis le répertoire courant : lancer `./bot` depuis un dossier différent d'une fois sur l'autre ne doit pas faire "perdre" la configuration, comme pour le reste du workspace (skills, mémoire...). Pour changer cet emplacement, définissez `WORKSPACE_DIR` comme une vraie variable d'environnement (`export WORKSPACE_DIR=...`), pas dans le fichier `.env` lui-même — sa propre localisation ne peut pas dépendre de son propre contenu.

Au tout premier lancement, si ce fichier n'existe pas encore, il est créé automatiquement avec le contenu de `.env.example` (embarqué dans le binaire) : éditez-le ensuite à l'emplacement indiqué ci-dessus, pas à la racine du dépôt. Variables déjà présentes dans l'environnement (réel) toujours prioritaires sur celles du fichier.

## Utilisation

```sh
go build -o bot .

./bot chat     # mode interactif console
./bot mail     # boucle de lecture/réponse automatique aux mails
```

En mode `chat` :
- `/exit` ou `/quit` — quitter
- `/reset` — vider l'historique de la conversation
- `/stats` — afficher le nombre de messages et l'estimation de tokens utilisés
- `/compact` — forcer une compaction immédiate (résumé + extraction mémoire, voir "Gestion du contexte" plus bas), comme si le seuil `CONTEXT_COMPACT_AT` venait d'être atteint. Utile pour alléger le contexte à la demande plutôt que d'attendre.

Chaque tour (réponse du modèle, y compris ses éventuels appels d'outils) s'exécute en tâche de fond : vous pouvez continuer à taper pendant qu'il tourne. Les lignes tapées pendant ce temps sont mises en file d'attente et traitées automatiquement, dans l'ordre, dès que le tour en cours se termine (commandes `/exit`, `/new`... comprises). Exception : si le modèle demande une confirmation pendant son tour (permission d'accès à un répertoire, mise en place du sandbox), c'est la ligne suivante que vous tapez qui lui répond, pas la file. Un Ctrl+C annule le tour en cours et vide la file d'attente.

## Gestion du contexte

La taille du contexte est estimée grossièrement (~4 caractères/token). Quand elle atteint la fraction `CONTEXT_COMPACT_AT` (0.95 par défaut) de `LLM_CONTEXT_TOKENS`, les messages les plus anciens sont résumés en un seul message via un appel au LLM (avec un vrai résumé structuré — voir plus bas pourquoi le format de cet appel est particulier), en conservant tels quels les `CONTEXT_KEEP_LAST` derniers messages. Cet appel de résumé ne propose aucun outil au modèle (il ne doit produire que du texte) : une réponse vide ou invalide (ex: un appel d'outil échappé en texte) fait échouer la compaction plutôt que de remplacer l'historique par un résumé creux — elle sera retentée au tour suivant (une seule fois par tour), sans jamais bloquer la réponse.

**Filet de sécurité de dernier recours** : si, même après (une tentative de) compaction, l'historique dépasse encore la fenêtre réelle du modèle (`LLM_CONTEXT_TOKENS`, pas seulement le seuil `CONTEXT_COMPACT_AT`), les messages les plus anciens sont supprimés sans passer par le LLM (aucun résumé, juste une coupe), jusqu'à repasser sous la limite — pour ne jamais envoyer une requête vouée à être rejetée par le serveur pour dépassement de contexte. Garde toujours au moins le dernier message, ne coupe jamais un groupe [appel d'outil + ses résultats], et le signale explicitement (`[avertissement: ...supprimé(s) sans résumé]`).

**Pourquoi l'appel de résumé a un format particulier** : rejouer la conversation à résumer avec ses vrais rôles (`user`/`assistant`/`tool`) pousse certains modèles à *continuer* la conversation (produire le tour suivant) plutôt qu'à obéir à l'instruction de résumer, faute de percevoir l'instruction système comme prioritaire sur le déroulé de la conversation — vérifié empiriquement. Le harnais sérialise donc la conversation à résumer en un unique message `user` (texte, sans les mots "Utilisateur"/"Assistant"), suivi d'une demande explicite de résumé — un schéma bien plus fiable pour obtenir un vrai résumé plutôt qu'une réponse de chat.

### Mémoire long terme (`MEMORY.md`)

En plus du résumé de compaction (qui ne vit que le temps de la conversation), un second appel LLM, **lancé en parallèle du résumé** (pas après — deux appels indépendants sur les mêmes messages, pas la peine de payer leur latence en série), cherche dans les messages compactés ce qui mérite de survivre au-delà de cette session, et l'ajoute, s'il trouve quelque chose, à `WORKSPACE_DIR/MEMORY.md`. Volontairement très sélectif : seulement les préférences de travail, contraintes systématiques, corrections de comportement et emplacements de fichiers récurrents — **jamais le contenu/sujet traité** (l'histoire d'un projet, ce qui a été produit...), qui n'aide en rien pour une tâche future différente et n'a pas sa place ici (le résumé de compaction s'en charge déjà, pour cette conversation). Le cas normal est qu'il n'y ait rien à ajouter. Contrepartie de la parallélisation : cet appel a lieu même si le résumé échoue ensuite (retenté au tour suivant), pas seulement en cas de succès.

Ce fichier est aussi consultable et modifiable directement par le modèle via `read_file`/`write_file` : demandez-lui explicitement de retenir quelque chose, il l'y écrira lui-même. Son contenu (tel qu'il était au démarrage du bot) est injecté directement dans le prompt système — pas seulement une invitation à le relire via `read_file` : plus fiable en pratique. Une modification faite en cours de session (par le modèle ou par une compaction) n'apparaît donc que dans le prompt système après un redémarrage, comme pour les skills.

**Activé aussi en mode mail** (`mailbot.Options.MemoryFile`, même fichier partagé que le mode chat) : à activer en connaissance de cause, puisque le corps d'un mail entrant est un contenu non fiable (voir `MAIL_ALLOW_FROM` plus bas) — l'extraction mémoire de la compaction y puise, donc un mail malveillant pourrait tenter d'y injecter une "mémoire" persistante qui influencerait ensuite le modèle bien au-delà de ce seul mail, y compris en mode chat.

## Mode mail — sécurité

- `MAIL_ALLOW_FROM` : liste blanche d'expéditeurs (adresses séparées par des virgules) autorisés à déclencher une réponse automatique. Un mail d'un expéditeur non listé est marqué comme lu mais jamais répondu. Vide = tout le monde autorisé.
- `MAIL_MAX_BODY_CHARS` : troncature du corps du mail transmis au LLM (protection contre les mails abusivement longs).

## Tool calling

Le LLM peut appeler des outils locaux si le serveur le supporte (`tools`/`tool_calls` de l'API OpenAI). Outils disponibles :

- `run_shell` — exécute une commande via `sh -c`. Disponible en mode chat, et en mode mail si `MAIL_TOOLS_ENABLED=true`. **Attention en mode mail** : le corps d'un mail entrant est un contenu non fiable (voir `MAIL_ALLOW_FROM` ci-dessous, lui-même vulnérable au spoofing de l'en-tête `From:`) — l'exposer ici est un vecteur d'injection de prompt → exécution de code arbitraire, accepté en connaissance de cause. Pas de repli silencieux : si `SANDBOX_USER_ENABLED=true`, `run_shell` n'est proposé en mode mail que si le sandbox a déjà été configuré ailleurs (mode mail n'a pas de console pour le mettre en place lui-même, voir ci-dessous), sinon il est simplement absent des outils disponibles ce cycle.
- `read_file` / `write_file` — lecture/écriture de fichiers locaux, soumis au système de permissions par répertoire ci-dessous. `write_file` reste **mode chat uniquement**. `read_file` accepte `offset`/`length` **en numéros de ligne** (1 = première ligne) pour lire un gros fichier par portions plutôt que de saturer le contexte : chaque appel tronqué indique en fin de résultat l'offset (de ligne) auquel reprendre. Refuse explicitement un fichier binaire (PDF, image, exécutable, archive...) plutôt que d'en renvoyer les octets bruts comme "texte" — détecté par la présence d'un octet NUL dans les premiers octets (heuristique standard, celle de `git`/`grep -I`).
- `http_get` — requête HTTP GET (http/https uniquement). Aucune protection SSRF (pas de filtrage des adresses privées/locales) : à activer en connaissance de cause si le LLM traite des entrées non fiables.
- `browser_fetch` — charge une URL dans un vrai navigateur **headless** (JavaScript exécuté) et retourne le DOM rendu, converti en texte comme `http_get`. Prévu pour les cas où `http_get` échoue ou revient bredouille (403, protection anti-bot, page qui ne se construit qu'après exécution de JavaScript côté client) — plus lent et plus lourd qu'une simple requête HTTP, donc un recours, pas un premier réflexe (le prompt du tool le précise au modèle). Firefox est utilisé en priorité s'il est disponible sur le `PATH` (`firefox`/`firefox-esr`), piloté via `geckodriver` (protocole WebDriver classique en HTTP, binaire explicitement précisé à `geckodriver` plutôt que de le laisser deviner — pas de dépendance externe au-delà de la bibliothèque standard) ; à défaut, un navigateur basé sur Chromium trouvé sur le `PATH` (`chromium`, `chromium-browser`, `google-chrome`, `microsoft-edge`...), piloté via son mode headless intégré (`--dump-dom`) ; à défaut encore, un Chromium téléchargé par [Playwright](https://playwright.dev/) (`npx playwright install chromium`, sous `~/.cache/ms-playwright/`), utile quand rien n'est installé nativement. Échoue explicitement si rien de tout ça n'est disponible — rien n'est empaqueté avec le bot. **Flatpak explicitement non géré** : un Firefox/Chromium installé en Flatpak n'est jamais détecté (recherche sur le `PATH`, puis Playwright). Testé empiriquement : inutile pour Firefox de toute façon (son sandbox bloque `geckodriver`, qui a besoin de partager un profil temporaire par `/tmp` entre deux processus de part et d'autre de la frontière du sandbox) ; un Chromium/Brave Flatpak, lui, fonctionne à la main mais s'est révélé peu fiable spécifiquement invoqué depuis le process du bot (`bwrap: Can't find source path .../doc/by-app/...: Permission denied`, jamais reproduit à la main) — vraisemblablement un souci de portail de documents D-Bus lié au contexte d'exécution du process appelant, hors de contrôle du bot ; d'où le repli Playwright, qui contourne Flatpak entièrement. Mêmes limites que `http_get` (pas de protection SSRF) ; ne s'exécute jamais sous le compte sandbox (l'URL est un simple argument de commande, jamais interprétée par un shell, donc sans le risque propre à `run_shell`).

**Vérifications anti-bot (Cloudflare et consorts)** : la page rendue est laissée jusqu'à 8s pour finir de se construire (`--virtual-time-budget` sur Chrome, une vraie pause sur Firefox/WebDriver) avant d'être capturée — utile pour un contenu chargé en différé ou une redirection JS. Si le résultat ressemble quand même à un écran de vérification ("Vérification de sécurité en cours", "Checking your browser"...), l'appel échoue explicitement plutôt que de renvoyer ce texte comme s'il s'agissait du contenu réel de la page.
- `request_directory_access` — seul moyen pour le modèle d'obtenir l'accès à un répertoire pour `read_file`/`write_file`/`run_shell`.

Activation : `CHAT_TOOLS_ENABLED` (défaut `true`) et `MAIL_TOOLS_ENABLED` (défaut `false` ; une fois activé, expose `read_file`/`http_get`/`browser_fetch`/`request_directory_access`/`run_shell` — jamais `write_file` — en mode mail). Autres réglages : `TOOLS_SHELL_TIMEOUT` (30s), `TOOLS_HTTP_TIMEOUT` (20s), `TOOLS_BROWSER_FETCH_TIMEOUT` (45s), `AGENT_MAX_STEPS` (8, nombre max d'allers-retours outils par tour avant abandon).

### Permissions sur les répertoires

Par défaut, **aucun répertoire n'est accessible** à `read_file`/`write_file`, à l'exception du workspace (voir plus bas) — ce verrou est appliqué dans le code (`internal/tools.DirPermissions`), pas seulement suggéré dans le prompt système : un chemin hors des répertoires autorisés est rejeté avant toute lecture/écriture disque, quoi que le modèle tente.

- Le modèle doit appeler `request_directory_access` pour demander l'accès à un répertoire.
- **Mode chat** : la demande déclenche une vraie question interactive dans la console (`autoriser ? [o/N]`) — c'est l'utilisateur, pas le modèle, qui décide.
- **Mode mail** : personne n'est disponible pour répondre en direct, donc toute demande y est automatiquement refusée.

**Si `SANDBOX_USER_ENABLED=true` (voir plus bas)**, la vérité vient du système : un répertoire est considéré autorisé si le compte sandbox `llm` y a *réellement* accès au niveau OS à l'instant du contrôle (`sudo -n -u llm test -r`/`-w`), pas simplement parce qu'un fichier prétend qu'il l'est. Accorder un répertoire (`request_directory_access`) le rend group-accessible à `llm` (voir `GrantDirectory` ci-dessous) : c'est cet état OS, et lui seul, qui fait foi ensuite pour `read_file`/`write_file` — y compris entre deux lancements du programme, sans dépendre d'un fichier de persistance. Un répertoire dont le groupe/les droits auraient été changés manuellement entre-temps redevient donc inaccessible sans qu'aucun fichier n'ait besoin d'être mis à jour.

**Si le sandbox est désactivé ou indisponible**, aucune identité séparée à interroger : le programme retombe sur une liste de répertoires explicitement accordés, **partagée** entre les invocations via un fichier de persistance (JSON, chemin fixe `.bot_allowed_dirs.json`, non configurable). Un accès accordé en mode chat y est ajouté et reste valable pour le reste de la session (et couvre les sous-répertoires) ; le mode mail relit ce même fichier avant chaque cycle de poll pour bénéficier des mêmes autorisations, mais **ne peut jamais lui-même en ajouter**.

### Sandbox utilisateur système pour `run_shell`

`SANDBOX_USER_ENABLED=true` fait exécuter `run_shell` sous un compte système dédié et restreint, `llm`, plutôt que sous l'utilisateur qui a lancé le programme — défense en profondeur en plus du filtre anti-commandes destructrices, pour le cas où celui-ci serait contourné.

À chaque lancement du mode chat, le programme vérifie si l'utilisateur courant peut déjà exécuter des commandes en tant que `llm` (`sudo -n -u llm true`). Si ce n'est pas le cas, il explique précisément ce qu'il va faire et demande confirmation avant d'exécuter (via `sudo`, qui demandera le mot de passe) :

1. Créer le groupe puis l'utilisateur système `llm` (compte système, sans mot de passe, sans connexion interactive) — via `dscl` sur macOS, `useradd`/`groupadd` sur Linux.
2. Ajouter l'utilisateur courant au groupe `llm`.
3. Installer une règle sudo limitée à l'utilisateur courant, autorisant uniquement `sudo -u llm` sans mot de passe (validée avec `visudo -c` avant toute installation).

**Le mode mail ne peut jamais effectuer cette configuration lui-même** (pas de console pour demander le mot de passe `sudo`) : il se contente de vérifier si le sandbox est déjà prêt (`sudo -n -u llm true`, sans jamais déclencher de prompt). S'il ne l'est pas, `run_shell` n'est simplement pas proposé en mode mail ce cycle — configurez-le au moins une fois via le mode chat pour qu'il devienne disponible aussi en mode mail.

**Aucun `chown` n'est jamais effectué.** Quand un répertoire est accordé au modèle (`request_directory_access`) alors que le sandbox est actif, seul son **groupe** Unix est changé vers `llm` (`chgrp`, récursif, plus bit setgid sur le répertoire racine accordé) — le propriétaire ne change jamais. Si ce changement de groupe échoue, l'accès est refusé plutôt que d'accorder silencieusement un accès non isolé.

Si le sandbox est demandé (`SANDBOX_USER_ENABLED=true`) mais que sa configuration échoue ou est refusée (mode chat), ou n'est pas encore prête (mode mail), `run_shell` n'est pas proposé du tout cette session — jamais de repli silencieux vers une exécution non isolée.

Nécessite macOS ou Linux (pas testé sur d'autres plateformes).

#### Git / SSH sous le sandbox

Le compte `llm` n'a pas de répertoire personnel : sans configuration, `git` y échoue ("dubious ownership", "Please tell me who you are") et toute commande git sur un dépôt distant en SSH bloque en attendant une confirmation de clé d'hôte impossible à donner. Au premier octroi du workspace au sandbox, le programme crée automatiquement `<workspace>/.sandbox-gitconfig` (identité recopiée depuis votre config git `--global`, `safe.directory=*`, `known_hosts` dédié en `StrictHostKeyChecking=accept-new`) et le pointe via `GIT_CONFIG_GLOBAL`.

Par défaut, `llm` n'a toujours **aucune clé SSH** : un `git push`/`pull` distant échoue avec `Permission denied (publickey)`. `SANDBOX_SSH_KEY` (chemin d'une clé privée SSH de l'utilisateur courant) donne à `llm` un accès en **lecture seule** à cette clé précise (ACL, `setfacl`/`chmod +a` — jamais de `chown`, jamais de copie de la clé), et l'utilise via `core.sshCommand -i`. **Implication de sécurité assumée** : le LLM peut alors s'authentifier en SSH avec la véritable identité de l'utilisateur (git push vers ses dépôts, etc.) — le sandbox n'isole plus les opérations SSH/git comme il isole le reste. Vide (défaut si aucune clé usuelle n'est trouvée dans `~/.ssh`) = comportement précédent, `run_shell` sous sandbox ne peut pas s'authentifier en SSH.

Ce fichier n'est jamais réécrit une fois créé (y compris s'il a été édité à la main) : pour appliquer `SANDBOX_SSH_KEY` sur un workspace déjà initialisé sans cette option, supprimez `<workspace>/.sandbox-gitconfig` pour le faire régénérer.

## Workspace et skills

Au démarrage, le bot crée (si besoin) un répertoire **workspace**, `WORKSPACE_DIR` (défaut `~/bot-workspace`), et son sous-répertoire `skills/`. Ce dernier contient toujours d'office une skill `creer-une-skill` (recréée si absente, jamais si elle existe déjà) qui explique au modèle lui-même le format ci-dessous, pour qu'il puisse déclarer de nouvelles skills sans documentation externe.

Ce workspace est **toujours accessible** en lecture/écriture à `read_file`/`write_file`, en mode chat comme en mode mail, sans passer par `request_directory_access` — contrairement aux autres répertoires (voir "Permissions sur les répertoires" ci-dessus), cet accès ne dépend pas du fichier partagé de répertoires autorisés et n'est jamais retiré par un rafraîchissement de celui-ci.

### Déclarer une skill

Une skill est TOUJOURS un sous-répertoire dédié de `WORKSPACE_DIR/skills/` contenant un fichier `SKILL.md` (avec un en-tête optionnel), jamais un fichier Markdown directement dans `skills/` — le sous-répertoire permet de regrouper la skill avec d'éventuels fichiers annexes dès le départ :

```
skills/ma-skill/SKILL.md
```

```markdown
---
name: ma-skill
description: ce que fait la skill et quand l'utiliser
---
Instructions détaillées pour le modèle, exemples, étapes à suivre...
```

Au démarrage, seuls le nom et la description de chaque skill trouvée sont ajoutés au prompt système (avec le chemin du fichier) : le corps complet n'est lu par le modèle que s'il appelle `read_file` sur ce chemin, pour ne pas saturer le contexte avec des skills non pertinentes à la conversation en cours.

## Limites connues

- Le client IMAP est minimal (LOGIN/SELECT/SEARCH UNSEEN/FETCH/STORE) : pas d'IDLE, pas d'OAuth2, pas de gestion avancée des dossiers.
- L'estimation du nombre de tokens est approximative (pas de tokenizer réel).
- En mode mail, les réponses sont envoyées automatiquement sans validation humaine.
- Le filtre anti-commandes destructrices (`rm -rf`, formatage, etc.) est heuristique (analyse du texte de la commande) : contournable par un attaquant motivé. La vraie protection reste de ne jamais exposer `run_shell` à un contexte non supervisé, et éventuellement d'activer `SANDBOX_USER_ENABLED`.
