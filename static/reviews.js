// @license magnet:?xt=urn:btih:0b31508aeb0634b347b8270c7bee4d411b5d4109&dn=agpl-3.0.txt AGPL-3.0-or-later
//
// Reordena las opiniones ya cargadas sin pedirle nada al servidor: las trece
// vienen en el propio HTML de la ficha. Sin este script los enlaces siguen
// funcionando, solo que recargan la pagina con el parametro resenas, que el
// servidor ya sabe atender.
(function () {
    'use strict';

    var lista = document.querySelector('.opiniones');
    var barra = document.querySelector('.ordenOpiniones');
    if (!lista || !barra) {
        return;
    }

    var articulos = Array.prototype.slice.call(lista.querySelectorAll('.opinion'));
    if (articulos.length < 2) {
        return;
    }

    function numero(articulo, campo) {
        return parseInt(articulo.dataset[campo], 10) || 0;
    }

    function ordenar(criterio) {
        var signo = 0;
        if (criterio === 'mejor') {
            signo = -1;
        } else if (criterio === 'peor') {
            signo = 1;
        }

        // El orden de partida no se toma del DOM: si la pagina se cargo con el
        // parametro resenas, lo que hay en el DOM ya viene ordenado por el
        // servidor y no habria forma de volver al orden de Amazon. Por eso cada
        // opinion trae su posicion original en data-relevancia.
        var copia = articulos.slice();
        copia.sort(function (a, b) {
            var diferencia = 0;
            if (signo !== 0) {
                diferencia = (numero(a, 'estrellas') - numero(b, 'estrellas')) * signo;
            }
            if (diferencia !== 0) {
                return diferencia;
            }
            return numero(a, 'relevancia') - numero(b, 'relevancia');
        });

        copia.forEach(function (articulo) {
            lista.appendChild(articulo);
        });
    }

    barra.addEventListener('click', function (evento) {
        var enlace = evento.target.closest('a[data-orden]');
        if (!enlace) {
            return;
        }
        evento.preventDefault();
        ordenar(enlace.dataset.orden);

        var enlaces = barra.querySelectorAll('a[data-orden]');
        for (var i = 0; i < enlaces.length; i++) {
            enlaces[i].className = enlaces[i] === enlace ? 'activo' : '';
        }

        // La URL refleja el orden para poder recargarla o compartirla, pero sin
        // anadir una entrada al historial por cada clic.
        history.replaceState(null, '', enlace.getAttribute('href'));
    });
})();
// @license-end
