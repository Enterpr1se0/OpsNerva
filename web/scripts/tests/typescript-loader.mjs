import { existsSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { extname } from 'node:path'
import { fileURLToPath } from 'node:url'

// Native stripping handles .ts; only component tests need the existing TS
// compiler for JSX. Nothing is emitted and no test/build cache is created.
export function resolve(specifier,context,next){
	if(specifier.startsWith('.')&&!extname(specifier)){
		for(const suffix of ['.ts','.tsx','/index.ts']){
			const candidate=new URL(specifier+suffix,context.parentURL)
			if(existsSync(candidate))return next(candidate.href,context)
		}
	}
	return next(specifier,context)
}

export async function load(url,context,next){
	if(url.endsWith('.tsx')){
		const ts=await import('typescript')
		const source=ts.transpileModule(await readFile(new URL(url),'utf8'),{
			fileName:fileURLToPath(url),
			compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX},
		}).outputText
		return{format:'module',source,shortCircuit:true}
	}
	return next(url,context)
}
