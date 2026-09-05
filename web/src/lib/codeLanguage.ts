const languageAliases:Record<string,string>={
	'c++':'cpp',
	'c#':'csharp',
	cs:'csharp',
	dockerfile:'bash',
	html:'xml',
	jsonl:'json',
	jsx:'javascript',
	js:'javascript',
	md:'markdown',
	ps1:'powershell',
	pwsh:'powershell',
	sh:'bash',
	shell:'bash',
	ts:'typescript',
	tsx:'typescript',
	yml:'yaml',
	zsh:'bash',
}

const extensionLanguages:Record<string,string>={
	bash:'bash',c:'c',cc:'cpp',conf:'ini',cpp:'cpp',cs:'csharp',css:'css',diff:'diff',env:'bash',go:'go',
	h:'c',hpp:'cpp',htm:'html',html:'html',ini:'ini',java:'java',js:'javascript',json:'json',jsonl:'jsonl',
	jsx:'jsx',kt:'kotlin',kts:'kotlin',less:'less',lua:'lua',md:'markdown',php:'php',pl:'perl',
	ps1:'powershell',py:'python',rb:'ruby',rs:'rust',scss:'scss',sh:'bash',sql:'sql',swift:'swift',
	toml:'toml',ts:'typescript',tsx:'tsx',xml:'xml',yaml:'yaml',yml:'yaml',zsh:'bash',
}

export function normalizeLanguage(language?:string){
	const normalized=language?.trim().toLowerCase()
	return normalized?(languageAliases[normalized]||normalized):undefined
}

export function languageFromCodeClass(className?:string){
	const language=className?.split(/\s+/).find(value=>value.startsWith('language-'))?.slice(9)
	return normalizeLanguage(language)
}

export function languageFromPath(path?:string){
	if(!path)return undefined
	const name=path.split(/[\\/]/).at(-1)?.toLowerCase().split(/[?#]/,1)[0]||''
	if(name==='dockerfile')return'dockerfile'
	if(name==='makefile')return'makefile'
	const extension=name.includes('.')?name.slice(name.lastIndexOf('.')+1):''
	return extensionLanguages[extension]
}

export function inferScriptLanguage(script:string){
	if(/(?:^|\n)\s*(?:#requires\b|param\s*\()|\$(?:ErrorActionPreference|PSVersionTable)\b|\b(?:Get|Set|New|Remove|Write|Start|Stop)-[A-Z][\w-]*/m.test(script))return'powershell'
	return'bash'
}
